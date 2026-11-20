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


# ==========================================================================
# Stream / back-pressure
# ==========================================================================


class TestStream:
    def test_slow_consumer_times_out(self):
        queue = BoundedQueue(1, slow_consumer_timeout=0.05)
        queue.put("a")
        with pytest.raises(QcError) as excinfo:
            queue.put("b")
        assert excinfo.value.category is ErrorCategory.QUEUE_TIMEOUT
        assert excinfo.value.exit_code == 4
        assert queue.stats.timed_out

    def test_peak_depth_bounded(self):
        def produce(_token):
            for value in range(500):
                yield value

        def consume(items):
            for _ in items:
                pass

        _, stats = run_stages(produce, consume, capacity=4)
        assert stats.peak_depth <= 4
        assert stats.total_put == 500

    def test_producer_error_propagates(self):
        def produce(_token):
            yield 1
            raise make_error(ErrorCategory.DECODE_FAILURE, "boom", offset=7)

        with pytest.raises(QcError) as excinfo:
            run_stages(produce, lambda items: list(items), capacity=2)
        assert excinfo.value.category is ErrorCategory.DECODE_FAILURE
        assert excinfo.value.details["offset"] == 7

    def test_cancellation_stops_producer(self):
        token = CancellationToken()
        stopped = []

        def produce(_token):
            try:
                for value in range(10**6):
                    yield value
            finally:
                stopped.append(True)

        def consume(items):
            next(items)
            token.cancel()
            raise RuntimeError("consumer failed")

        with pytest.raises(RuntimeError):
            run_stages(produce, consume, capacity=2, token=token)
        assert stopped


# ==========================================================================
# Decode
# ==========================================================================


class TestDecode:
    def test_clean_decode_is_ordered_and_unique(self, fixture_root):
        blocks = list(
            decode_video_blocks(fixture_root / "program_h264.bin", Codec.H264, Fraction(25), block_frames=8)
        )
        frames = [frame for block in blocks for frame in block.frames]
        assert len(frames) == FRAMES
        assert [frame.pts for frame in frames] == list(range(FRAMES))
        assert all(len(block) <= 8 for block in blocks)

    def test_retry_recovers_and_matches_clean(self, fixture_root):
        path = fixture_root / "program_h264.bin"
        clean = {
            frame.pts: pixel_hash(frame.array)
            for block in decode_video_blocks(path, Codec.H264, Fraction(25))
            for frame in block.frames
        }
        model = LossModel(SEED, 0.1).configure(FRAMES)
        stats = DecodeStats()
        lossy = {}
        for block in decode_video_blocks(
            path, Codec.H264, Fraction(25), policy=LossPolicy(model=model), stats=stats, total_units=FRAMES
        ):
            for frame in block.frames:
                lossy[frame.pts] = (pixel_hash(frame.array), frame.recovered, frame.source)
        dropped = [i for i in range(FRAMES) if model.drops(i)]
        assert dropped, "loss model must drop at least one unit"
        assert stats.recovered_packets == len(dropped)
        assert stats.recovery_rate == 1
        for pts in dropped:
            assert lossy[pts][1] is True
            assert lossy[pts][2] == "retry"
            assert lossy[pts][0] == clean[pts]

    def test_hard_loss_conceals_and_is_not_recovered(self, fixture_root):
        path = fixture_root / "program_h264.bin"
        model = LossModel(SEED, 0.1, 1.0).configure(FRAMES)
        stats = DecodeStats()
        frames = []
        for block in decode_video_blocks(
            path, Codec.H264, Fraction(25), policy=LossPolicy(model=model), stats=stats, total_units=FRAMES
        ):
            frames.extend(block.frames)
        concealed = [frame for frame in frames if frame.source == "concealed"]
        assert concealed
        assert all(frame.recovered is False for frame in concealed)
        assert stats.unrecoverable_packets > 0
        assert stats.concealed_frames == len(concealed)

    def test_loss_model_is_count_accurate(self):
        for total in (10, 50, 200):
            model = LossModel(7, 0.1).configure(total)
            assert len([i for i in range(total) if model.drops(i)]) == round(0.1 * total)

    def test_audio_channel_alignment_and_tail_padding(self, fixture_root):
        reader = AudioReader(fixture_root / "program_audio.wav", channels=1)
        block = next(reader.read_blocks(1024))
        assert block.samples.shape == (1024, 1)
        tail, padded = reader.read_window(reader.frames_total - 10, 100)
        assert tail.shape == (100, 1)
        assert padded == 90
        assert np.allclose(tail[10:], 0.0)
        reader.close()

    def test_resample_exact_length(self):
        one_second_down = np.zeros((48000, 2), dtype=np.float32)
        one_second_up = np.zeros((44100, 2), dtype=np.float32)
        assert resample_window(one_second_down, 48000, 44100).shape == (44100, 2)
        assert resample_window(one_second_up, 44100, 48000).shape == (48000, 2)


# ==========================================================================
# Render
# ==========================================================================


class TestRender:
    def test_source_spans_record_byte_ranges(self, fixture_root):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "render.json"))
        renderer = Renderer(timeline)
        frames = list(renderer.frames())
        assert len(frames) == FRAMES
        for frame in frames:
            video_spans = [span for span in frame.spans if span.track == "v"]
            assert len(video_spans) == 1
            span = video_spans[0]
            assert span.source_size > 0
            assert span.source_index == frame.number
            assert span.recovered is False

    def test_sync_error_detects_known_offset(self, fixture_root):
        document = _simple_document()
        document["clips"][1]["start_ticks"] = 3600  # audio 40 ms late
        document["clips"][1]["duration_ticks"] -= 3600
        timeline = load_timeline(_write_timeline(fixture_root, document, "sync.json"))
        tracker = SyncTracker()
        for frame in Renderer(timeline).frames():
            tracker.add_frame(frame.time_seconds, frame.array)
            tracker.add_audio(frame.audio)
        error_ms, lag = tracker.sync_error_ms()
        assert error_ms is not None
        assert abs(float(error_ms) - 40.0) <= 1.0
        assert lag == 1

    def test_resampling_track_renders(self, fixture_root):
        document = _simple_document(audio="program_audio_44100.wav", audio_rate=44100)
        timeline = load_timeline(_write_timeline(fixture_root, document, "resample.json"))
        tracker = SyncTracker()
        for frame in Renderer(timeline).frames():
            tracker.add_frame(frame.time_seconds, frame.array)
            tracker.add_audio(frame.audio)
        error_ms, _ = tracker.sync_error_ms()
        assert error_ms is not None
        assert abs(float(error_ms)) <= 50.0


# ==========================================================================
# Pipeline
# ==========================================================================


class TestPipeline:
    def test_full_run_meets_targets(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "pipe.json"))
        result = run_pipeline(
            timeline,
            tmp_path,
            RunParams(seed=SEED, drop_rate=0.02, block_frames=8, queue_blocks=1),
        )
        report = result.report.to_json()
        assert report["exit_code"] == 0
        assert report["sync_error_ms_p95"] <= 20.0
        assert report["packet_recovery_rate"] >= 0.90
        assert report["latency_ms_p95"] <= 500.0
        assert report["peak_queue_frames"] <= 8
        assert report["frame_hash_mismatches"] == 0
        assert result.frames_written == FRAMES
        manifest = read_manifest(result.manifest_path)
        assert manifest["frames_written"] == FRAMES
        assert manifest["complete_blocks"] == (FRAMES + 7) // 8

    def test_rerun_is_byte_identical(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "rerun.json"))
        params = RunParams(seed=SEED, drop_rate=0.02, block_frames=8, queue_blocks=1)
        first = run_pipeline(timeline, tmp_path / "a", params)
        second = run_pipeline(timeline, tmp_path / "b", params)
        hashes_a = [block["hash"] for block in read_manifest(first.manifest_path)["blocks"]]
        hashes_b = [block["hash"] for block in read_manifest(second.manifest_path)["blocks"]]
        assert hashes_a == hashes_b
        assert first.report.to_json()["frames_rendered"] == second.report.to_json()["frames_rendered"]

    def test_resume_from_complete_blocks(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "resume.json"))
        params = RunParams(seed=SEED, drop_rate=0.02, block_frames=8, queue_blocks=1)
        out = tmp_path / "run"
        first = run_pipeline(timeline, out, params)
        original = read_manifest(first.manifest_path)
        reference = [block["hash"] for block in original["blocks"]]

        keep = len(original["blocks"]) // 2
        truncated = dict(original)
        truncated["blocks"] = original["blocks"][:keep]
        truncated["complete_blocks"] = keep
        (out / "manifest.json").write_text(json.dumps(truncated), encoding="utf-8")
        for block in original["blocks"][keep:]:
            for entry in block["entries"]:
                (out / "frames" / entry["file"]).unlink(missing_ok=True)

        resumed = run_pipeline(timeline, out, params, resume=True)
        assert resumed.resumed_from_block == keep
        final = read_manifest(resumed.manifest_path)
        assert [block["hash"] for block in final["blocks"]] == reference
        assert resumed.report.frame_hash_mismatches == 0

    def test_resume_rejects_changed_inputs(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "reject.json"))
        params = RunParams(seed=SEED, block_frames=8, queue_blocks=1)
        run_pipeline(timeline, tmp_path, params)
        with pytest.raises(QcError) as excinfo:
            run_pipeline(timeline, tmp_path, RunParams(seed=SEED, drop_rate=0.05, block_frames=8, queue_blocks=1), resume=True)
        assert excinfo.value.category is ErrorCategory.CHECKSUM_MISMATCH
        assert excinfo.value.exit_code == 2

    def test_truncated_input_exits_3_with_gap(self, fixture_root, tmp_path):
        document = _simple_document(video="program_h264_truncated.bin")
        timeline = load_timeline(_write_timeline(fixture_root, document, "trunc.json"))
        result = run_pipeline(timeline, tmp_path, RunParams(seed=SEED, block_frames=8, queue_blocks=1))
        report = result.report.to_json()
        assert report["exit_code"] == 3
        assert report["concealed_frames"] > 0
        assert report["max_gap_ms"] > 0
        concealed = [
            span
            for block in read_manifest(result.manifest_path)["blocks"]
            for entry in block["entries"]
            for span in entry["sources"]
            if span.get("concealed")
        ]
        assert concealed and all(span["recovered"] is False for span in concealed)

    def test_verify_manifest_detects_tampering(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "verify.json"))
        result = run_pipeline(timeline, tmp_path, RunParams(seed=SEED, block_frames=8, queue_blocks=1))
        clean = verify_manifest(result.manifest_path)
        assert clean["mismatches"] == 0 and clean["exit_code"] == 0
        manifest = read_manifest(result.manifest_path)
        frame_file = tmp_path / "frames" / manifest["blocks"][0]["entries"][0]["file"]
        frame_file.write_bytes(frame_file.read_bytes()[:64])  # truncate mid-file
        tampered = verify_manifest(result.manifest_path)
        assert tampered["mismatches"] >= 1
        assert tampered["exit_code"] == 3

    def test_output_failure_is_resource_error(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "outfail.json"))
        blocker = tmp_path / "frames"
        blocker.write_text("not a directory", encoding="utf-8")
        with pytest.raises(QcError) as excinfo:
            run_pipeline(timeline, tmp_path, RunParams(seed=SEED, block_frames=8, queue_blocks=1))
        assert excinfo.value.exit_code == 4


# ==========================================================================
# CLI
# ==========================================================================


class TestCli:
    def test_scan_and_verify(self, fixture_root, tmp_path, capsys):
        from bcastqc.cli import main

        code = main(["scan", "--stream", str(fixture_root / "program_h264.bin")])
        assert code == 0
        payload = json.loads(capsys.readouterr().out)
        assert payload["access_units"] == FRAMES

        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "cli.json"))
        run_pipeline(timeline, tmp_path, RunParams(seed=SEED, block_frames=8, queue_blocks=1))
        code = main(["verify", "--manifest", str(tmp_path / "manifest.json")])
        assert code == 0
        payload = json.loads(capsys.readouterr().out)
        assert payload["frames"] == FRAMES and payload["mismatches"] == 0

    def test_invalid_timeline_exit_code(self, fixture_root, tmp_path, capsys):
        from bcastqc.cli import main

        document = _simple_document()
        document["tracks"][0]["depends_on"] = ["a"]
        document["tracks"][1]["depends_on"] = ["v"]
        path = _write_timeline(fixture_root, document, "cli_cycle.json")
        code = main(["run", "--timeline", str(path), "--out", str(tmp_path)])
        assert code == 2
        payload = json.loads(capsys.readouterr().out)
        assert payload["error"]["category"] == "timeline.cycle"


# ==========================================================================
# Failure classification and edge inputs
# ==========================================================================


class TestFailureClassification:
    def test_corrupt_access_unit_is_illegal_and_not_recovered(self, fixture_root):
        from bcastqc.bitstream import validate_access_unit

        path = fixture_root / "program_h264_corrupt.bin"
        data = path.read_bytes()
        units = AnnexBParser(Codec.H264).split(data)
        assert len(units) == FRAMES
        problems = [validate_access_unit(Codec.H264, data[u.offset:u.end], strict=True) for u in units]
        assert any(problem is not None for problem in problems)

        stats = DecodeStats()
        frames = []
        for block in decode_video_blocks(
            path, Codec.H264, Fraction(25), block_frames=8, stats=stats
        ):
            frames.extend(block.frames)
        assert stats.illegal_units >= 1
        # concealment covers the gap up to (and including) the next keyframe;
        # if the corrupt unit is after the last keyframe, the resync itself
        # does not happen, but the gap is still reported
        concealed = [frame for frame in frames if frame.source == "concealed"]
        assert concealed and all(frame.recovered is False for frame in concealed)
        # the stream stays time-consistent: every frame slot is present
        assert [frame.pts for frame in frames] == list(range(FRAMES))

    def test_truncated_tail_is_concealed_once(self, fixture_root, tmp_path):
        document = _simple_document(video="program_h264_truncated.bin")
        timeline = load_timeline(_write_timeline(fixture_root, document, "tail.json"))
        result = run_pipeline(timeline, tmp_path, RunParams(seed=SEED, block_frames=8, queue_blocks=1))
        report = result.report.to_json()
        assert report["exit_code"] == 3
        assert report["max_gap_ms"] > 0
        assert report["packet_recovery_rate"] == 1.0  # nothing was counted as lost

    def test_total_loss_reports_zero_recovery_and_exits_3(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "total.json"))
        result = run_pipeline(
            timeline,
            tmp_path,
            RunParams(seed=SEED, drop_rate=0.2, hard_loss_rate=1.0, block_frames=8, queue_blocks=1),
        )
        report = result.report.to_json()
        assert report["packet_recovery_rate"] == 0.0
        assert report["concealed_frames"] > 0
        assert report["exit_code"] == 3

    def test_empty_timeline_yields_zero_frames(self, fixture_root, tmp_path):
        document = _simple_document()
        document["clips"] = []
        timeline = load_timeline(_write_timeline(fixture_root, document, "empty.json"))
        result = run_pipeline(timeline, tmp_path, RunParams(seed=SEED, block_frames=8, queue_blocks=1))
        assert result.report.frames_rendered == 0
        assert result.report.exit_code == 2

    def test_zero_length_audio_tail_padded(self, fixture_root, tmp_path):
        # clip longer than the audio file: the tail must be zero-padded, not
        # read past EOF or drift
        document = _simple_document(duration_ticks=90000 * 4)  # 4 s, audio is 3 s
        timeline = load_timeline(_write_timeline(fixture_root, document, "long.json"))
        renderer = Renderer(timeline)
        frames = list(renderer.frames())
        assert renderer.stats.tail_padded_samples > 0
        for frame in frames:
            if frame.audio is not None:
                assert frame.audio.shape[0] > 0

    def test_negative_track_offset_trims_start(self, fixture_root, tmp_path):
        document = _simple_document()
        document["tracks"][1]["offset_ticks"] = 9000  # audio starts 100 ms later
        timeline = load_timeline(_write_timeline(fixture_root, document, "neg.json"))
        renderer = Renderer(timeline)
        frames = list(renderer.frames())
        # before the source starts the mix is silence, never a missing block
        assert all(frame.audio is not None for frame in frames)
        head = np.concatenate([frame.audio for frame in frames[:2]])
        assert np.max(np.abs(head)) < 1e-6
        assert np.max(np.abs(np.concatenate([frame.audio for frame in frames[5:10]]))) > 0.01


# ==========================================================================
# Alignment measurement and acceptance scenario
# ==========================================================================


class TestAlignment:
    def test_duplicates_detected_for_concealment(self, fixture_root, tmp_path):
        from bcastqc.pipeline import clean_reference

        base = load_timeline(_write_timeline(fixture_root, _simple_document(), "align_base.json"))
        corrupt_doc = _simple_document(video="program_h264_corrupt.bin")
        corrupt = load_timeline(_write_timeline(fixture_root, corrupt_doc, "align_corrupt.json"))
        reference = clean_reference(base, block_frames=8)
        result = run_pipeline(
            corrupt,
            tmp_path,
            RunParams(seed=SEED, block_frames=8, queue_blocks=1, reference=True, **reference.as_params()),
        )
        manifest = read_manifest(result.manifest_path)
        alignment = manifest["alignment"]
        assert alignment["duplicated_count"] > 0
        assert alignment["duplicate_runs"]
        placement = alignment["placement"]
        assert placement["displaced_count"] == alignment["duplicated_count"]
        assert all(index >= 0 for index in placement["displaced"])

    def test_audio_offset_is_measured_independently(self, fixture_root, tmp_path):
        from bcastqc.pipeline import clean_reference

        aligned = load_timeline(_write_timeline(fixture_root, _simple_document(), "al.json"))
        shifted_doc = _simple_document()
        shifted_doc["clips"][1]["start_ticks"] = 3600
        shifted_doc["clips"][1]["duration_ticks"] -= 3600
        shifted = load_timeline(_write_timeline(fixture_root, shifted_doc, "sh.json"))
        reference = clean_reference(aligned, block_frames=8)
        result = run_pipeline(
            shifted,
            tmp_path,
            RunParams(seed=SEED, block_frames=8, queue_blocks=1, reference=True, **reference.as_params()),
        )
        placement = read_manifest(result.manifest_path)["alignment"]["av_placement"]
        assert placement["video_lag_frames"] == 0
        assert placement["audio_lag_frames"] == -1
        assert placement["av_offset_frames"] == -1

    def test_quantiles_are_deterministic(self):
        from bcastqc.report import quantile

        values = [5.0, 1.0, 3.0, 2.0, 4.0]
        assert quantile(values, 0.5) == 3.0
        assert quantile(values, 0.95) == pytest.approx(4.8)
        assert quantile([], 0.95) == 0.0


class TestAcceptanceScenario:
    def test_demo_meets_targets(self, tmp_path):
        from bcastqc.demo import run_demo

        out = tmp_path / "demo"
        result = run_demo(
            seed=SEED,
            drop_rate=0.02,
            queue_frames=8,
            consumer_delay_ms=0.0,
            out=out,
            frames=FRAMES,
        )
        assert result["sync_error_ms_p95"] <= 20.0
        assert result["packet_recovery_rate"] >= 0.90
        assert result["latency_ms_p95"] <= 500.0
        assert result["peak_queue_frames"] <= 8
        assert result["frame_hash_mismatches"] == 0
        assert result["exit_code"] == 0
        # the failure cases each produce their documented exit code
        cases = result["failure_cases"]
        assert cases["truncated"]["exit_code"] == 3
        assert cases["hard_loss"]["exit_code"] == 3
        assert cases["hard_loss"]["packet_recovery_rate"] == 0.0
        assert cases["cycle"]["exit_code"] == 2
        assert cases["audio_offset"]["av_offset_frames"] == -1
        corrupt = cases["corrupt"]
        assert corrupt["exit_code"] == 3
        assert corrupt["illegal_units"] >= 1
        # a corrupt unit is never counted as a recovered packet
        assert corrupt["recovered_packets"] == 0
        assert corrupt["duplicated_count"] >= 1
        # excluded track is reported, never silently composed
        assert "unused_slate" in result["excluded_tracks"]
        # verify command agrees with the run
        verify = verify_manifest(out / "run" / "manifest.json")
        assert verify["mismatches"] == 0 and verify["exit_code"] == 0


class TestDegenerateInputs:
    def test_empty_stream_is_input_error(self, tmp_path):
        (tmp_path / "empty.bin").write_bytes(b"")
        document = {
            "time_base": "1/90000",
            "frame_rate": "25/1",
            "tracks": [{"id": "v", "kind": "video", "path": "empty.bin", "codec": "h264"}],
            "clips": [{"id": "c", "track": "v", "start_ticks": 0, "source_in_ticks": 0, "duration_ticks": 90000}],
        }
        path = tmp_path / "timeline.json"
        path.write_text(json.dumps(document), encoding="utf-8")
        timeline = load_timeline(path)
        with pytest.raises(QcError) as excinfo:
            run_pipeline(timeline, tmp_path / "out", RunParams(block_frames=8, queue_blocks=1))
        assert excinfo.value.exit_code == 2

    def test_zero_frame_timeline_exits_2(self, fixture_root, tmp_path):
        document = _simple_document()
        document["clips"] = []
        timeline = load_timeline(_write_timeline(fixture_root, document, "zero.json"))
        result = run_pipeline(timeline, tmp_path, RunParams(block_frames=8, queue_blocks=1))
        assert result.report.frames_rendered == 0
        assert result.report.exit_code == 2

    def test_slow_consumer_is_resource_error(self, fixture_root, tmp_path):
        timeline = load_timeline(_write_timeline(fixture_root, _simple_document(), "slow.json"))
        with pytest.raises(QcError) as excinfo:
            run_pipeline(
                timeline,
                tmp_path,
                RunParams(block_frames=8, queue_blocks=1, consumer_delay_ms=300, slow_consumer_timeout=0.1),
            )
        assert excinfo.value.category is ErrorCategory.QUEUE_TIMEOUT
        assert excinfo.value.exit_code == 4

    def test_audio_only_timeline_renders(self, fixture_root, tmp_path):
        document = {
            "time_base": "1/90000",
            "frame_rate": "25/1",
            "tracks": [{"id": "a", "kind": "audio", "path": "program_audio.wav",
                        "sample_rate": 48000, "channels": 2}],
            "clips": [{"id": "c", "track": "a", "start_ticks": 0, "source_in_ticks": 0, "duration_ticks": 90000}],
        }
        timeline = load_timeline(_write_timeline(fixture_root, document, "aonly.json"))
        result = run_pipeline(timeline, tmp_path, RunParams(block_frames=8, queue_blocks=1))
        assert result.report.frames_rendered == 25
        assert result.report.exit_code == 0


class TestConcealmentBounds:
    def test_concealment_never_overlaps_resync_keyframe(self):
        from bcastqc.decode import VideoDecoder, DecodedFrame

        decoder = VideoDecoder(Codec.H264, Fraction(25))
        # fabricate a last good frame at pts 0
        decoder._last_frame = DecodedFrame(
            pts=0, time=Fraction(0), array=np.zeros((4, 4, 3), dtype=np.uint8),
            keyframe=True, source="decode", source_offset=0, source_size=10, au_index=0,
        )
        decoder._next_emit = 1
        # a 10-frame gap but the keyframe sits at pts 4: only 3 frames fit
        concealed = list(decoder.conceal(10, until_pts=4))
        assert [frame.pts for frame in concealed] == [1, 2, 3]
        assert decoder._next_emit == 4


class TestExactAudioCadence:
    @pytest.mark.parametrize("frame_rate,ticks", [("25/1", 180000), ("30000/1001", 180000), ("24/1", 225000)])
    def test_total_audio_matches_duration(self, fixture_root, frame_rate, ticks):
        document = _simple_document(frame_rate=frame_rate, duration_ticks=ticks)
        document["tracks"][0]["frame_rate"] = "25/1"  # source cadence differs
        timeline = load_timeline(_write_timeline(fixture_root, document, f"cad_{ticks}_{frame_rate.replace('/', '_')}.json"))
        frames = list(Renderer(timeline).frames())
        total = sum(frame.audio.shape[0] for frame in frames if frame.audio is not None)
        expected = round(Fraction(ticks, 90000) * 48000)
        assert total == expected

    def test_ntsc_partial_frame_is_included_and_clipped(self, fixture_root):
        document = _simple_document(frame_rate="30000/1001", duration_ticks=180000)
        document["tracks"][0]["frame_rate"] = "25/1"
        timeline = load_timeline(_write_timeline(fixture_root, document, "ntsc.json"))
        renderer = Renderer(timeline)
        assert renderer.frame_count() == 60  # ceil(2 s * 29.97)
        frames = list(renderer.frames())
        assert frames[-1].audio.shape[0] < frames[-2].audio.shape[0]


class TestPlacementRobustness:
    def test_repeated_reference_pictures_do_not_false_positive(self):
        from bcastqc.report import measure_placement

        class FakeTracker:
            frame_hashes = ["a", "b", "b", "c", "b", "b"]

        reference = ["a", "b", "b", "c", "b", "b"]
        result = measure_placement(FakeTracker(), reference)
        assert result["displaced_count"] == 0
        assert result["missing_count"] == 0

    def test_duplicate_picture_is_displaced(self):
        from bcastqc.report import measure_placement

        class FakeTracker:
            # frame 2 lost: output repeats 'b' twice and never shows 'c'
            frame_hashes = ["a", "b", "b", "b"]

        reference = ["a", "b", "c", "d"]
        result = measure_placement(FakeTracker(), reference)
        assert result["displaced_count"] >= 1
        assert 2 in result["displaced"]

    def test_synthesised_picture_is_missing(self):
        from bcastqc.report import measure_placement

        class FakeTracker:
            frame_hashes = ["a", "zzz", "c"]

        result = measure_placement(FakeTracker(), ["a", "b", "c"])
        assert result["missing"] == [1]
