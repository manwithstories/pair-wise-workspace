"""Decode helpers: single-AU keyframes, bounded quality scoring, tail probe."""

from __future__ import annotations

import pytest

from mediacore.bitstream import chunk_bytes, scan
from mediacore.decode import (
    decode_keyframe, decode_tail_probe, score_quality,
)
from mediacore.fixtures import FixtureSpec, encode_video, make_frame


@pytest.fixture(scope="module")
def stream(tmp_path_factory):
    path = tmp_path_factory.mktemp("dec") / "v.h264"
    spec = FixtureSpec(duration_s=2, bitrate=600_000, seed=11)
    data = encode_video(path, spec)
    units = [u for u in scan(chunk_bytes(data, 4096), codec="h264")
             if not hasattr(u, "units")]
    return spec, data, units


def test_keyframe_au_decodes_standalone(stream):
    _, _, units = stream
    for unit in [u for u in units if u.keyframe][:2]:
        picture = decode_keyframe(unit, "h264")
        assert picture is not None
        assert picture.key
        assert picture.y.shape == (180, 320)


def test_quality_matches_reference_and_is_bounded(stream):
    spec, _, units = stream
    stats = score_quality(units, "h264", lambda i: make_frame(i, spec)[0],
                          limit=20, cache_bytes=1 << 20)
    payload = stats.to_dict()
    assert payload["frames"] == 20
    assert payload["finite"] is True
    assert payload["psnr_db"] > 25.0
    assert payload["frame_cache"]["bytes_current"] <= 1 << 20
    assert payload["frame_cache"]["inserted"] == 20


def test_quality_without_cache_still_scores(stream):
    spec, _, units = stream
    stats = score_quality(units, "h264", lambda i: make_frame(i, spec)[0], limit=5)
    assert stats.to_dict()["frame_cache"] is None
    assert stats.frames == 5


def test_tail_probe_accepts_complete_stream(stream):
    _, _, units = stream
    truncated, reason, index = decode_tail_probe(units, "h264")
    assert truncated is False
    assert reason == ""
    assert index is None


def test_tail_probe_detects_cut_picture(tmp_path):
    spec = FixtureSpec(duration_s=2, bitrate=600_000, seed=11)
    data = encode_video(tmp_path / "v.h264", spec)
    last_sc = data.rfind(b"\x00\x00\x01")
    # remove a good chunk of the final picture's payload, keeping the header
    cut = data[: last_sc + 3 + 8]
    units = [u for u in scan(chunk_bytes(cut, 2048), codec="h264")
             if not hasattr(u, "units")]
    truncated, reason, index = decode_tail_probe(units, "h264")
    assert truncated is True
    assert "start code" in reason or "decode" in reason
    assert index == units[-1].index


def test_dropped_payload_is_reread_from_source(stream, tmp_path):
    _, data, units = stream
    path = tmp_path / "v.h264"
    path.write_bytes(data)
    unit = units[0]
    expected = unit.data
    unit.source_path = str(path)
    unit.release_payload()
    assert unit.payload() == expected
    assert unit.size == len(expected)


def test_payload_without_source_raises(stream):
    from mediacore.errors import MediaCorruptError

    _, _, units = stream
    unit = units[1]
    unit.release_payload()
    with pytest.raises(MediaCorruptError):
        unit.payload()
