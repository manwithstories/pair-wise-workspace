"""End-to-end and unit tests for the broadcast QC pipeline.

Fixtures are generated once per session (they are deterministic) and shared;
tests that mutate output directories use ``tmp_path``.
"""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

import numpy as np
import pytest

from bcastqc.bitstream import (
    AnnexBParser,
    Av1ObuParser,
    Codec,
    KeyframeEntry,
    KeyframeIndex,
    StreamScanner,
    compute_index,
    detect_codec,
    validate_index,
)
from bcastqc.clock import MediaTime, MediaWindow, Rounding, TimeBase, round_rational
from bcastqc.decode import (
    AudioReader,
    DecodeStats,
    LossModel,
    LossPolicy,
    decode_video_blocks,
    resample_window,
)
from bcastqc.errors import CATEGORY_SPECS, ErrorCategory, QcError, make_error
from bcastqc.fixtures import FixtureSpec, generate_fixture_set
from bcastqc.pipeline import RunParams, read_manifest, run_pipeline, verify_manifest
from bcastqc.render import Renderer, SyncTracker, pixel_hash
from bcastqc.stream import BoundedQueue, CancellationToken, pipeline as run_stages
from bcastqc.timeline import find_cycle, load_timeline, topological_order

FRAMES = 50
SEED = 41


# ==========================================================================
# Shared fixtures
# ==========================================================================


@pytest.fixture(scope="session")
def fixture_root(tmp_path_factory) -> Path:
    root = tmp_path_factory.mktemp("fixtures")
    spec = FixtureSpec(seed=SEED, frames=FRAMES, gop=25)
    generate_fixture_set(root, spec)
    return root


@pytest.fixture(scope="session")
def fixture_spec() -> FixtureSpec:
    return FixtureSpec(seed=SEED, frames=FRAMES, gop=25)


def _write_timeline(root: Path, document: dict, name: str = "timeline.json") -> Path:
    path = root / name
    path.write_text(json.dumps(document), encoding="utf-8")
    return path


def _simple_document(
    video: str = "program_h264.bin",
    audio: str = "program_audio.wav",
    *,
    frames: int = FRAMES,
    frame_rate: str = "25/1",
    audio_rate: int = 48000,
    duration_ticks: int | None = None,
) -> dict:
    ticks = duration_ticks if duration_ticks is not None else int(Fraction(frames) / Fraction(frame_rate) * 90000)
    return {
        "time_base": "1/90000",
        "frame_rate": frame_rate,
        "width": 160,
        "height": 90,
        "output_sample_rate": 48000,
        "tracks": [
            {"id": "v", "kind": "video", "path": video, "codec": "h264"},
            {"id": "a", "kind": "audio", "path": audio, "sample_rate": audio_rate, "channels": 2},
        ],
        "clips": [
            {"id": "cv", "track": "v", "start_ticks": 0, "source_in_ticks": 0, "duration_ticks": ticks},
            {"id": "ca", "track": "a", "start_ticks": 0, "source_in_ticks": 0, "duration_ticks": ticks},
        ],
    }


# ==========================================================================
# Clock
# ==========================================================================


class TestClock:
    def test_rounding_modes(self):
        assert round_rational(Fraction(5, 2), Rounding.HALF_EVEN) == 2
        assert round_rational(Fraction(7, 2), Rounding.HALF_EVEN) == 4
        assert round_rational(Fraction(5, 2), Rounding.HALF_UP) == 3
        assert round_rational(Fraction(-5, 2), Rounding.HALF_UP) == -3
        assert round_rational(Fraction(7, 3), Rounding.FLOOR) == 2
        assert round_rational(Fraction(7, 3), Rounding.CEIL) == 3
        assert round_rational(Fraction(-7, 3), Rounding.TRUNC) == -2

    def test_timebase_is_reduced(self):
        assert TimeBase(2, 180000) == TimeBase(1, 90000)
        assert TimeBase.parse("1/90000").ticks_per_second == 90000

    def test_rescale_is_exact(self):
        base = TimeBase(1, 90000)
        assert base.rescale(90000, TimeBase(1, 48000)) == 48000
        assert base.rescale(1, TimeBase(1, 90000)) == 1

    def test_no_drift_over_many_rescales(self):
        source = TimeBase(1, 90000)
        target = TimeBase(1, 48000)
        for ticks in range(0, 90000 * 10, 7):
            seconds = Fraction(ticks, 90000)
            samples = source.rescale(ticks, target)
            # exact rational rescaling: the error is bounded by half a target
            # sample and never accumulates from one tick to the next
            assert abs(Fraction(samples, 48000) - seconds) <= Fraction(1, 48000) / 2

    def test_window_intersection(self):
        base = TimeBase(1, 1000)
        a = MediaWindow(MediaTime(0, base), MediaTime(500, base))
        b = MediaWindow(MediaTime(400, base), MediaTime(500, base))
        overlap = a.intersect(b)
        assert overlap is not None
        assert overlap.start.ticks == 400 and overlap.duration.ticks == 100
        assert MediaWindow(MediaTime(0, base), MediaTime(100, base)).intersect(
            MediaWindow(MediaTime(200, base), MediaTime(100, base))
        ) is None


# ==========================================================================
# Errors
# ==========================================================================


class TestErrors:
    def test_every_category_has_spec(self):
        for category in ErrorCategory:
            assert category in CATEGORY_SPECS
            assert CATEGORY_SPECS[category].exit_code in (2, 3, 4)

    def test_serialisation_round_trip(self):
        error = make_error(ErrorCategory.INDEX_OUT_OF_RANGE, "bad offset", offset=12)
        payload = error.to_dict()
        assert payload["recoverable"] is False
        assert payload["strategy"] == "none"
        rebuilt = QcError.from_dict(payload)
        assert rebuilt.category is ErrorCategory.INDEX_OUT_OF_RANGE
        assert rebuilt.details["offset"] == 12

    def test_recovery_policy(self):
        assert make_error(ErrorCategory.PACKET_LOSS, "x").recoverable is True
        assert make_error(ErrorCategory.PATH_ESCAPE, "x").recoverable is False
        assert make_error(ErrorCategory.QUEUE_TIMEOUT, "x").exit_code == 4
        assert make_error(ErrorCategory.TIMELINE_CYCLE, "x").exit_code == 2
        assert make_error(ErrorCategory.DECODE_FAILURE, "x").exit_code == 3


# ==========================================================================
# Bitstream
# ==========================================================================


class TestBitstream:
    @pytest.mark.parametrize(
        "role,codec",
        [("video_h264", Codec.H264), ("video_h265", Codec.H265), ("video_av1", Codec.AV1)],
    )
    def test_detect_and_split(self, fixture_root, role, codec):
        path = fixture_root / {"video_h264": "program_h264.bin", "video_h265": "program_h265.bin", "video_av1": "program_av1.bin"}[role]
        data = path.read_bytes()
        assert detect_codec(data[:4096]) is codec
        parser = Av1ObuParser() if codec is Codec.AV1 else AnnexBParser(codec)
        units = parser.split(data)
        assert len(units) == FRAMES
        assert units[-1].end == len(data)
        assert units[0].offset == 0
        assert any(unit.is_keyframe for unit in units)

    def test_incremental_matches_bulk(self, fixture_root):
        data = (fixture_root / "program_h264.bin").read_bytes()
        units = AnnexBParser(Codec.H264).split(data)
        scanner = StreamScanner(Codec.H264, max_buffer=1 << 20)
        streamed = []
        for start in range(0, len(data), 997):
            streamed.extend(scanner.feed(data[start:start + 997]))
        streamed.extend(scanner.finish())
        assert [(u.offset, u.size, u.is_keyframe) for u in streamed] == [
            (u.offset, u.size, u.is_keyframe) for u in units
        ]

    def test_index_validation_reports_positions(self, fixture_root):
        path = fixture_root / "program_h264.bin"
        data = path.read_bytes()
        units = AnnexBParser(Codec.H264).split(data)
        bad = KeyframeIndex(
            entries=[
                KeyframeEntry(offset=units[2].offset),
                KeyframeEntry(offset=units[2].offset),      # duplicate
                KeyframeEntry(offset=10**9),                 # out of range
                KeyframeEntry(offset=units[1].offset),       # out of order
            ]
        )
        validation = validate_index(bad, len(data), keyframes=units)
        kinds = [issue.kind for issue in validation.issues]
        assert "duplicate" in kinds and "out_of_range" in kinds and "out_of_order" in kinds
        assert all(issue.source_byte is not None for issue in validation.issues)
        assert validation.recovered_index is not None
        assert len(validation.recovered_index.entries) == len(compute_index(units).entries)

    def test_index_reject_policy(self, fixture_root):
        data = (fixture_root / "program_h264.bin").read_bytes()
        units = AnnexBParser(Codec.H264).split(data)
        bad = KeyframeIndex(entries=[KeyframeEntry(offset=0), KeyframeEntry(offset=0)])
        validation = validate_index(bad, len(data), keyframes=units, policy="reject")
        assert validation.accepted is False
        assert validation.recovered_index is None

    def test_empty_stream_rejected(self):
        with pytest.raises(QcError) as excinfo:
            detect_codec(b"")
        assert excinfo.value.category is ErrorCategory.STREAM_EMPTY


# ==========================================================================
# Timeline
# ==========================================================================


class TestTimeline:
    def test_cycle_reports_path(self, fixture_root):
        document = _simple_document()
        document["tracks"][0]["depends_on"] = ["a"]
        document["tracks"][1]["depends_on"] = ["v"]
        path = _write_timeline(fixture_root, document, "cycle.json")
        with pytest.raises(QcError) as excinfo:
            load_timeline(path)
        assert excinfo.value.category is ErrorCategory.TIMELINE_CYCLE
        cycle = excinfo.value.details["cycle"]
        assert cycle[0] == cycle[-1] and len(cycle) >= 3
        assert excinfo.value.exit_code == 2

    def test_find_cycle_helper(self):
        assert find_cycle(["a", "b"], {"a": ("b",), "b": ("a",)}) == ["a", "b", "a"]
        assert find_cycle(["a", "b"], {"a": ("b",), "b": ()}) is None
        assert topological_order(["a", "b"], {"a": ("b",), "b": ()}) == ["b", "a"]

    def test_overlap_rejected_with_interval(self, fixture_root):
        document = _simple_document()
        document["clips"] = [
            {"id": "c1", "track": "v", "start_ticks": 0, "source_in_ticks": 0, "duration_ticks": 90000},
            {"id": "c2", "track": "v", "start_ticks": 60000, "source_in_ticks": 0, "duration_ticks": 90000},
        ]
        path = _write_timeline(fixture_root, document, "overlap.json")
        with pytest.raises(QcError) as excinfo:
            load_timeline(path)
        assert excinfo.value.category is ErrorCategory.OVERLAP
        assert excinfo.value.details["overlap_ticks"] == 30000
        assert excinfo.value.details["overlap_start_ticks"] == 60000

    def test_unreferenced_track_excluded_not_composed(self, fixture_root):
        document = _simple_document()
        document["tracks"].append(
            {"id": "spare", "kind": "video", "path": "program_h265.bin", "codec": "h265"}
        )
        path = _write_timeline(fixture_root, document, "spare.json")
        timeline = load_timeline(path)
        assert timeline.excluded_tracks == ["spare"]
        assert "spare" not in timeline.active_tracks()
        assert any("excluded" in note for note in timeline.notes)
        with pytest.raises(QcError) as excinfo:
            load_timeline(path, strict=True)
        assert excinfo.value.category is ErrorCategory.TRACK_UNREFERENCED

    def test_path_escape_rejected(self, fixture_root):
        document = _simple_document()
        document["tracks"][0]["path"] = "../../../etc/passwd"
        path = _write_timeline(fixture_root, document, "escape.json")
        with pytest.raises(QcError) as excinfo:
            load_timeline(path)
        assert excinfo.value.category is ErrorCategory.PATH_ESCAPE

    def test_exact_sample_mapping(self, fixture_root):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "exact.json"))
        assert timeline.ticks_to_samples(90000, 48000) == 48000
        assert timeline.ticks_to_samples(90000, 44100) == 44100
        assert timeline.samples_to_ticks(44100, 44100) == 90000
