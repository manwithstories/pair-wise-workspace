from __future__ import annotations

import struct
from pathlib import Path

import pytest

from mediaqc.bmff import BoxReader
from mediaqc.cli import probe_path
from mediaqc.codec import parse_hvcc
from mediaqc.sniff import SniffError, sniff_file
from mediaqc.tracks import parse_mp4_tracks


def _tracks(path: Path):
    scan = BoxReader(path).scan()
    with path.open("rb") as handle:
        tracks, findings, movie_timescale = parse_mp4_tracks(handle, scan)
    return scan, tracks, findings, movie_timescale


def test_sniffs_all_supported_containers(fixture_dir: Path) -> None:
    assert sniff_file(fixture_dir / "clean_h264_aac.mp4").container == "mp4"
    assert sniff_file(fixture_dir / "clean_h264_aac.mov").container == "mov"
    assert sniff_file(fixture_dir / "clean_audio.wav").container == "wav"
    assert sniff_file(fixture_dir / "clean_video.avi").container == "avi"


def test_unknown_container_reports_reason(fixture_dir: Path) -> None:
    with pytest.raises(SniffError, match="unsupported_container"):
        sniff_file(fixture_dir / "unsupported.bin")
    result = probe_path(fixture_dir / "unsupported.bin")
    assert result.container == "unsupported"
    assert result.findings[0].rule_id == "UNSUPPORTED_CONTAINER"


def test_unknown_ftyp_brand_is_not_treated_as_mp4(tmp_path: Path) -> None:
    path = tmp_path / "unknown-brand.bin"
    payload = b"heic" + b"\x00\x00\x00\x00" + b"heic"
    path.write_bytes(struct.pack(">I4s", 8 + len(payload), b"ftyp") + payload)
    with pytest.raises(SniffError, match="unsupported_container|unsupported ftyp"):
        sniff_file(path)


def test_clean_mp4_track_facts_and_timeline(fixture_dir: Path) -> None:
    result = probe_path(fixture_dir / "clean_h264_aac.mp4")
    assert result.container == "mp4"
    assert len(result.tracks) == 2
    assert result.track_duration("video") == pytest.approx(10000.0)
    assert result.track_duration("audio") == pytest.approx(10000.0)
    assert result.drift_ms == pytest.approx(0.0)
    assert result.findings == []
    video, audio = result.tracks
    assert video.codec.startswith("avc1.")
    assert video.width == 320 and video.height == 180
    assert video.codec_details["sps"][0]["profile_idc"] == 66
    assert audio.codec == "mp4a.40.2"
    assert audio.sample_rate == 48000 and audio.channels == 2
    assert audio.codec_details["aac"]["sample_rate"] == 48000
    assert len(video.samples) == 300
    assert video.samples[1].dts == 3000


def test_leading_free_and_zero_sized_mdat(fixture_dir: Path) -> None:
    clean = sniff_file(fixture_dir / "clean_h264_aac.mp4")
    assert clean.offset == 8
    path = fixture_dir / "zero_size_mdat.mp4"
    scan, tracks, findings, _ = _tracks(path)
    assert len(scan.mdat_ranges) == 1
    assert scan.mdat_ranges[0][1] == path.stat().st_size
    assert len(tracks) == 2 and not findings


def test_co64_and_64bit_mdat_length(fixture_dir: Path) -> None:
    path = fixture_dir / "co64_clean.mp4"
    scan, tracks, findings, _ = _tracks(path)
    assert len(scan.mdat_ranges) == 1
    assert tracks[1].table_facts["chunk_offset_count"] == 1
    assert tracks[1].chunk_ranges[0][0] >= scan.mdat_ranges[0][0]
    assert tracks[1].chunk_ranges[0][1] <= scan.mdat_ranges[0][1]
    assert not findings


def test_truncated_mdat_is_rejected_by_boundary_check(fixture_dir: Path) -> None:
    result = probe_path(fixture_dir / "truncated_mdat.mp4")
    rules = {finding.rule_id for finding in result.findings}
    assert "BOX_BOUNDARY_INVALID" in rules
    assert "NO_MDAT" in rules


def test_moof_marks_unsupported_layout(fixture_dir: Path) -> None:
    result = probe_path(fixture_dir / "moof_fragmented.mp4")
    assert result.metadata["unsupported_layout"] is True
    assert "UNSUPPORTED_LAYOUT" in {item.rule_id for item in result.findings}


def test_excessive_box_nesting_is_reported(tmp_path: Path) -> None:
    nested = b""
    for _ in range(36):
        nested = struct.pack(">I4s", 8 + len(nested), b"moov") + nested
    ftyp_payload = b"isom" + b"\x00\x00\x00\x00" + b"isom"
    path = tmp_path / "deep.mp4"
    path.write_bytes(struct.pack(">I4s", 8 + len(ftyp_payload), b"ftyp")
                     + ftyp_payload + nested)
    scan = BoxReader(path).scan()
    assert "BOX_BOUNDARY_INVALID" in {item.rule_id for item in scan.findings}


def test_tail_garbage_has_offset(fixture_dir: Path) -> None:
    result = probe_path(fixture_dir / "tail_garbage.mp4")
    item = next(finding for finding in result.findings if finding.rule_id == "TAIL_GARBAGE")
    assert item.offset == fixture_dir.joinpath("tail_garbage.mp4").stat().st_size - 4


def test_multiple_sps_are_all_retained(fixture_dir: Path) -> None:
    result = probe_path(fixture_dir / "multi_sps.mp4")
    assert len(result.tracks[0].codec_details["sps"]) == 2
    assert "MULTIPLE_SPS" in {finding.rule_id for finding in result.findings}


def test_hvcc_configuration_and_sps_are_parsed() -> None:
    payload = bytes.fromhex(
        "0101000000000000000000005d00000001000000000f012100010018"
        "4201010100000300000300000300000300005da00a080b54"
    )
    details, findings = parse_hvcc(payload)
    assert not findings
    assert details["profile_idc"] == 1
    assert details["level_idc"] == 93
    assert details["sps"][0]["width"] == 320
    assert details["sps"][0]["height"] == 180


def test_large_sample_count_is_counted_without_expansion(fixture_dir: Path, tmp_path: Path) -> None:
    source = (fixture_dir / "clean_h264_aac.mp4").read_bytes()
    marker = source.index(b"stsz")
    box_start = marker - 4
    count_offset = box_start + 8 + 8
    patched = bytearray(source)
    patched[count_offset:count_offset + 4] = struct.pack(">I", 150_000)
    path = tmp_path / "large-count.mp4"
    path.write_bytes(patched)
    result = probe_path(path)
    video = result.tracks[0]
    assert video.sample_count == 150_000
    assert video.samples == []
    assert video.samples_truncated is True
    assert "SAMPLE_COUNT_HIGH" in {finding.rule_id for finding in result.findings}


def test_wav_odd_chunk_padding_and_avi_streams(fixture_dir: Path) -> None:
    odd = probe_path(fixture_dir / "odd_data.wav")
    assert odd.container == "wav"
    assert odd.tracks[0].kind == "audio"
    assert "ODD_DATA_CHUNK" in {finding.rule_id for finding in odd.findings}
    avi = probe_path(fixture_dir / "clean_video.avi")
    assert avi.container == "avi"
    assert [track.kind for track in avi.tracks] == ["video", "audio"]
    assert avi.drift_ms == pytest.approx(0.0)
    assert avi.findings == []
