"""Tests for keyframe indexing and audio sampling."""

from __future__ import annotations

import pytest

from aircheck.audio import inspect_audio
from aircheck.fixtures import bad_index_asset
from aircheck.keyframes import build_index, nearest_keyframe, validate_index
from aircheck.probe import probe


class TestKeyframes:
    @pytest.mark.parametrize("fixture_name", ["multi_mp4", "multi_mov", "multi_mkv"])
    def test_healthy_index_seeks_hit_every_keyframe(self, fixture_name, request):
        fixture = request.getfixturevalue(fixture_name)
        result = probe(fixture.path)
        index = build_index(fixture.path, result)
        report = validate_index(fixture.path, index, probe_result=result)
        assert report.keyframe_count > 0
        assert report.hit_rate == 1.0, report.problems
        assert report.problems == []
        assert report.ok

    def test_index_entries_ascend_per_stream(self, multi_mov):
        indexes = build_index(multi_mov.path)
        for index in indexes:
            timestamps = [entry.pts_ticks for entry in index.entries]
            assert timestamps == sorted(timestamps)

    def test_nearest_keyframe_binary_search(self, multi_mov):
        index = build_index(multi_mov.path)[0]
        last = index.entries[-1]
        assert nearest_keyframe(index, last.pts_ticks) == last
        assert nearest_keyframe(index, -1) is None
        middle = index.entries[len(index.entries) // 2]
        found = nearest_keyframe(index, middle.pts_ticks + 1)
        assert found is not None and found.pts_ticks <= middle.pts_ticks + 1

    def test_broken_cue_table_reports_all_three_problems(self, workdir):
        fixture = bad_index_asset(workdir / "bad_cues.mkv")
        result = probe(fixture.path)
        report = validate_index(
            fixture.path, build_index(fixture.path, result),
            probe_result=result, probe_seeks=False,
        )
        kinds = {problem.kind for problem in report.problems}
        assert {"container_duplicate", "container_out_of_order", "container_out_of_range"} <= kinds
        assert not report.ok

    def test_audio_only_asset_has_empty_index_not_error(self, workdir):
        fixture = bad_index_asset(workdir / "audio_only.mkv")
        assert build_index(fixture.path) == []


class TestAudio:
    def test_channels_rate_and_peak(self, multi_mp4):
        result = probe(multi_mp4.path, scan_packets=False)
        report = inspect_audio(multi_mp4.path, result, sample_stride=4)
        assert len(report.streams) == 2
        rates = sorted(stream.sample_rate for stream in report.streams)
        assert rates == [44100, 48000]
        for stream in report.streams:
            assert stream.channels == 2
            assert 0.1 < stream.peak <= 1.0
            assert stream.decode_failures == 0
            assert report.ok

    def test_silent_asset_reports_a_silence_interval(self, silent_mov):
        result = probe(silent_mov.path, scan_packets=False)
        report = inspect_audio(
            silent_mov.path, result, sample_stride=1, min_silence_seconds=0.1
        )
        stream = report.streams[0]
        # The fixture alternates one second of tone with one second of silence,
        # so the peak comes from the tone half while a silent interval exists.
        assert 0.1 < stream.peak <= 1.0
        assert stream.silence, "expected at least one silent interval"
        longest = max(stream.silence, key=lambda interval: interval.duration_ticks)
        assert longest.duration_ticks >= 400_000_000
        # The silent run must start after the first (tone) second.
        assert longest.start_ticks >= 900_000_000

    def test_sampling_stride_reduces_work(self, multi_mov):
        result = probe(multi_mov.path, scan_packets=False)
        full = inspect_audio(multi_mov.path, result, sample_stride=1)
        sampled = inspect_audio(multi_mov.path, result, sample_stride=16)
        assert sampled.streams[0].frames_decoded <= full.streams[0].frames_decoded
        assert sampled.sample_stride == 16

    def test_unusual_sample_rate_is_flagged(self, workdir):
        from aircheck.fixtures import EncodeSpec, encode_asset

        fixture = encode_asset(
            workdir / "odd_rate.mov",
            EncodeSpec(format_key="mov", video_streams=1, audio_streams=1,
                       sample_rates=(22050,), duration_ms=400),
        )
        result = probe(fixture.path, scan_packets=False)
        report = inspect_audio(fixture.path, result, sample_stride=1)
        kinds = {anomaly.kind for anomaly in report.streams[0].anomalies}
        assert "unusual_sample_rate" in kinds

    def test_video_only_asset_yields_empty_audio_report(self, workdir):
        from aircheck.fixtures import EncodeSpec, encode_asset

        fixture = encode_asset(
            workdir / "video_only.mp4",
            EncodeSpec(format_key="mp4", video_streams=1, audio_streams=0,
                       duration_ms=300),
        )
        result = probe(fixture.path, scan_packets=False)
        report = inspect_audio(fixture.path, result)
        assert report.streams == []
        assert report.ok


class TestPcmVerification:
    def test_pcm_fixture_roundtrips_with_zero_sample_difference(self, workdir):
        from aircheck.fixtures import build_pcm_fixture
        from aircheck.mapping import resolve_mapping
        from aircheck.pcm import compare_pcm
        from aircheck.pipeline import Pipeline, PipelineConfig

        fixture = build_pcm_fixture(workdir / "pcm_source.mov")
        source_probe = probe(fixture.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / "pcm_copy.mkv"
        stage = Pipeline(
            source_probe, mapping, output,
            PipelineConfig(segment_ticks=500_000_000, target_format="mkv"),
        ).run()
        assert stage.ok, stage.errors
        report = compare_pcm(
            fixture.path, stage.output, source_probe, probe(stage.output)
        )
        assert report.compared_streams == 2
        assert report.skipped_streams == 0
        assert report.ok
        for check in report.checks:
            assert check.identical
            assert check.max_difference == 0.0
            assert check.frames_compared > 0

    def test_non_pcm_audio_is_skipped_not_failed(self, multi_mp4):
        from aircheck.pcm import compare_pcm

        source_probe = probe(multi_mp4.path)
        report = compare_pcm(
            multi_mp4.path, multi_mp4.path, source_probe, source_probe
        )
        assert report.ok
        assert report.compared_streams == 0
        assert report.skipped_streams == 2
        assert all("non-PCM" in check.reason for check in report.checks)
