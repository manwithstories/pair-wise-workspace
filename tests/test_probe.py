"""Tests for probing, damage classification and index health."""

from __future__ import annotations

import shutil
from pathlib import Path

import pytest

from aircheck.fixtures import (
    bad_index_asset,
    corrupt_asset,
    disorder_timestamp_asset,
    truncate_asset,
    wrap_timestamp_asset,
)
from aircheck.probe import probe


@pytest.fixture(scope="module")
def healthy(multi_mp4):
    return multi_mp4.path


def test_probe_reports_streams_and_duration(healthy):
    result = probe(healthy)
    assert len(result.streams) == 4
    assert len(result.video_streams) == 2
    assert len(result.audio_streams) == 2
    assert result.duration_ticks and result.duration_ticks > 0
    assert result.issues == []
    assert result.has_index


def test_probe_flags_audio_priming_without_calling_it_negative(multi_mkv):
    result = probe(multi_mkv.path)
    assert "negative" not in {issue.kind for issue in result.issues}


def test_probe_missing_file_is_an_input_error(workdir: Path):
    with pytest.raises(Exception) as info:
        probe(workdir / "nope.mp4")
    assert info.value.category.value == "input"


def test_probe_empty_file_is_an_input_error(workdir: Path):
    empty = workdir / "empty.mp4"
    empty.write_bytes(b"")
    with pytest.raises(Exception) as info:
        probe(empty)
    assert "empty" in info.value.message


def test_truncated_asset_is_classified_not_raised(healthy, workdir: Path):
    fixture = truncate_asset(healthy, workdir / "trunc.mp4", 0.6)
    result = probe(fixture.path, decode_check=True)
    assert "truncated" in {issue.kind for issue in result.issues}


def test_truncated_mov_without_moov_is_still_classified(multi_mov, workdir: Path):
    # A non-faststart MOV keeps moov at the tail; cutting it removes the index
    # entirely and the demuxer refuses to open the file.
    source = workdir / "tail_index.mov"
    shutil.copy(multi_mov.path, source)
    fixture = truncate_asset(source, workdir / "tail_trunc.mov", 0.5)
    result = probe(fixture.path)
    kinds = {issue.kind for issue in result.issues}
    assert "truncated" in kinds or "corrupt" in kinds
    assert result.truncated or result.unreadable


def test_corrupt_video_packet_is_detected(healthy, workdir: Path):
    fixture = corrupt_asset(healthy, workdir / "corrupt.mp4")
    result = probe(fixture.path, decode_check=True)
    assert "corrupt" in {issue.kind for issue in result.issues}


def test_wrapped_timestamps_are_reported_as_wrap(workdir: Path):
    fixture = wrap_timestamp_asset(workdir / "wrap.mkv")
    result = probe(fixture.path)
    assert "wrap" in {issue.kind for issue in result.issues}
    assert result.observed_duration_ticks is None


def test_disordered_timestamps_are_reported(workdir: Path):
    fixture = disorder_timestamp_asset(workdir / "disorder.mkv")
    result = probe(fixture.path)
    assert "non_monotonic" in {issue.kind for issue in result.issues}


def test_broken_cue_table_is_reported_as_index_corrupt(workdir: Path):
    fixture = bad_index_asset(workdir / "bad_index.mkv")
    result = probe(fixture.path)
    problems = set(result.index_info["problems"])
    assert {"duplicate_cue", "cue_out_of_order", "cue_past_eof"} <= problems
    assert "index_corrupt" in {issue.kind for issue in result.issues}


def test_mkv_without_cues_is_no_index(multi_mkv, workdir: Path):
    # An MKV keeps its Cues element at the tail; truncating it drops the index.
    fixture = truncate_asset(multi_mkv.path, workdir / "no_cues.mkv", 0.9)
    result = probe(fixture.path)
    kinds = {issue.kind for issue in result.issues}
    assert "no_index" in kinds
    assert "truncated" in kinds


def test_probe_is_repeatable(healthy):
    first = probe(healthy, scan_packets=False).to_dict()
    second = probe(healthy, scan_packets=False).to_dict()
    assert first == second


def test_packet_counts_cover_every_media_stream(healthy):
    result = probe(healthy)
    assert set(result.packet_counts) == {0, 1, 2, 3}
    assert all(count > 0 for count in result.packet_counts.values())
