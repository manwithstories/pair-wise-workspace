"""Chunk-independent scanning, start-code handling and truncation offsets."""

from __future__ import annotations

import pytest

from mediacore.bitstream import chunk_bytes, scan
from mediacore.fixtures import FixtureSpec, encode_video


def _collect(data: bytes, codec: str, chunk: int = 7):
    units, report = [], None
    for item in scan(chunk_bytes(data, chunk), codec=codec):
        if hasattr(item, "units"):
            report = item
        else:
            units.append(item)
    return units, report


@pytest.mark.parametrize("codec", ["h264", "h265"])
def test_offsets_and_sizes_do_not_depend_on_chunking(tmp_path, codec):
    data = encode_video(tmp_path / f"v.{codec}", FixtureSpec(
        codec=codec, duration_s=1, bitrate=400_000, seed=5))
    reference = [(u.offset, u.size, u.keyframe) for u in _collect(data, codec, 4096)[0]]
    for chunk in (1, 2, 3, 5, 64, len(data)):
        units, _ = _collect(data, codec, chunk)
        assert [(u.offset, u.size, u.keyframe) for u in units] == reference


def test_every_annexb_unit_has_a_start_code(tiny_units):
    for unit in tiny_units:
        assert unit.data.startswith(b"\x00\x00\x01")
        assert unit.nal_types
        assert unit.keyframe == any(t in (5,) for t in unit.nal_types)


def test_keyframes_are_reported_at_expected_intervals(tiny_units):
    keyed = [u.index for u in tiny_units if u.keyframe]
    assert keyed[0] == 0
    assert all(b - a == 15 for a, b in zip(keyed, keyed[1:]))


def test_cut_inside_a_start_code_reports_that_offset(tmp_path):
    data = encode_video(tmp_path / "v.h264", FixtureSpec(duration_s=1, seed=3))
    # end the file just after a start code chosen in the final GOP
    tail = data.rfind(b"\x00\x00\x01", 0, len(data))
    cut = data[:tail + 3]
    units, report = _collect(cut, "h264", 13)
    # The incomplete unit is located at its own start, and the exact cut point
    # (the payload-less start code) is reported separately.
    assert report.truncated_start_code_offset == tail
    assert report.truncated_tail_offset == units[-1].offset
    assert "start code" in report.truncated_tail_reason
    assert units[-1].truncated and not units[-1].complete
    codes = [i.code for i in report.issues]
    assert "truncated_tail" in codes
    assert "truncated_start_code" in codes


def test_leading_garbage_is_reported_with_offset(tmp_path):
    data = encode_video(tmp_path / "v.h264", FixtureSpec(duration_s=1, seed=3))
    units, report = _collect(b"\xde\xad\xbe\xef" + data, "h264", 11)
    assert units[0].offset == 5  # zero padding before 00 00 01 is not part of a NAL
    assert any(i.code == "garbage_skipped" and i.offset == 0 for i in report.issues)


def test_truncated_start_code_at_eof_is_located(tiny_bytes):
    units, report = _collect(tiny_bytes[:-1] + b"\x00\x00\x01", "h264", 64)
    assert report.truncated_tail_offset is not None
    assert report.truncated_tail_offset <= len(tiny_bytes)


def test_av1_units_are_delimited_and_keyframes_found(tmp_path):
    data = encode_video(tmp_path / "v.obu", FixtureSpec(
        codec="av1", duration_s=2, bitrate=600_000, seed=5))
    units, report = _collect(data, "av1", 4096)
    assert report.units == len(units) == 60
    assert report.keyframes >= 2
    # each temporal unit begins with a temporal delimiter OBU (type 2)
    assert all(u.data[0:1] and (u.data[0] >> 3) & 0x0F == 2 for u in units)


def test_av1_cut_payload_is_reported_at_obu_offset(tmp_path):
    data = encode_video(tmp_path / "v.obu", FixtureSpec(
        codec="av1", duration_s=1, bitrate=600_000, seed=5))
    units, report = _collect(data[:-17], "av1", 555)
    assert report.truncated_tail_offset is not None
    assert not units[-1].complete
    assert any(i.code.startswith("truncated_obu") for i in report.issues)


def test_scan_rejects_unknown_codec():
    with pytest.raises(ValueError):
        list(scan([b""], codec="vp9"))


def test_empty_input_reports_no_start_code():
    units, report = _collect(b"", "h264")
    assert units == []
    assert any(i.code == "empty_stream" for i in report.issues)
