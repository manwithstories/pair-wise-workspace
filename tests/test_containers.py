from __future__ import annotations

import struct
from pathlib import Path

import pytest

from mediaprobe.boxes import find_box, parse_boxes, read_box_data
from mediaprobe.checks import check_paths
from mediaprobe.errors import MediaError
from mediaprobe.probe import probe_file
from mediaprobe.riff import parse_wav
from mediaprobe.samplegen import generate_samples
from mediaprobe.isobmff import _parse_elst, _parse_mdhd, _parse_mvhd


@pytest.fixture(scope="module")
def fixtures(tmp_path_factory) -> Path:
    destination = tmp_path_factory.mktemp("fixtures")
    generate_samples(destination, 14, 7)
    return destination


def test_acceptance_probe_summary(fixtures: Path) -> None:
    asset = probe_file(fixtures / "trailer_1080_av.mp4")
    assert asset.track_count == 2
    assert asset.duration_ms == 12000
    video, audio = asset.tracks
    assert (video.profile, video.level) == ("High", "4.0")
    assert (audio.sample_rate, audio.channels) == (48000, 2)
    assert find_box(parse_boxes(asset.path), "moov") is not None


def test_elst_timecode_and_fragment_durations(fixtures: Path) -> None:
    mov = probe_file(fixtures / "trailer_1080_timecode.mov")
    assert mov.format == "mov"
    assert mov.tracks[0].duration_source == "elst"
    assert mov.tracks[2].handler == "timecode"
    fragmented = probe_file(fixtures / "trailer_fragmented.mp4")
    assert fragmented.fragmented
    assert fragmented.duration_ms == 12000
    assert fragmented.tracks[0].duration_source == "fragments"
    assert len(fragmented.tracks[0].sample_ranges) == 2


def test_wav_odd_padding_and_extensible_subformat(fixtures: Path) -> None:
    wav = parse_wav(fixtures / "sfx_ambience.wav")
    assert wav.extensible_subformat == 1
    assert wav.codec == "pcm"
    assert (wav.sample_rate, wav.channels, wav.valid_bits) == (48000, 2, 16)
    assert any(chunk["id"] == "JUNK" and chunk["size"] % 2 for chunk in wav.chunks)
    assert wav.duration_ms == 3000


def test_check_acceptance_counts(fixtures: Path, tmp_path: Path) -> None:
    report = check_paths(fixtures, "profiles/delivery.json", 40)
    assert (len(report.assets), report.passed, report.rejected) == (14, 11, 3)
    assert 5 <= report.issue_count <= 9
    drift = next(issue for issue in report.flat_issues() if issue["code"] == "DUR_DRIFT")
    assert (drift["track_id"], drift["delta_ms"], drift["limit_ms"]) == (2, -48, 40)


def test_probe_error_codes(tmp_path: Path) -> None:
    empty = tmp_path / "empty.mp4"
    empty.write_bytes(b"")
    with pytest.raises(MediaError) as caught:
        probe_file(empty)
    assert caught.value.code == "FILE_EMPTY"
    unknown = tmp_path / "unknown.bin"
    unknown.write_bytes(b"not a media container" * 4)
    with pytest.raises(MediaError) as caught:
        probe_file(unknown)
    assert caught.value.code == "MAGIC_UNKNOWN"
    truncated = tmp_path / "truncated.mp4"
    truncated.write_bytes(b"\x00\x00\x01\x00ftypisom")
    with pytest.raises(MediaError) as caught:
        probe_file(truncated)
    assert caught.value.code == "FILE_TRUNCATED"


def test_fragment_without_default_duration(fixtures: Path, tmp_path: Path) -> None:
    source = fixtures / "trailer_fragmented.mp4"
    data = bytearray(source.read_bytes())
    with open(source, "rb") as fh:
        roots = parse_boxes(fh)
        tfhd = find_box(roots, "tfhd")
        trex_boxes = []
        assert tfhd is not None
        flags = int.from_bytes(read_box_data(fh, tfhd)[:4], "big")
        for root in roots:
            for box_item in root.children:
                if box_item.type == "mvex":
                    trex_boxes.extend(box_item.children_of("trex"))
    flags &= 0xFFFFFF ^ 0x000008
    data[tfhd.data_offset:tfhd.data_offset + 4] = flags.to_bytes(4, "big")
    for trex in trex_boxes:
        data[trex.data_offset + 12:trex.data_offset + 16] = b"\x00" * 4
    target = tmp_path / "fragment_no_default.mp4"
    target.write_bytes(data)
    asset = probe_file(target)
    assert any(track.fragment_error == "FRAG_NO_DEFAULT_DURATION" for track in asset.tracks)


def test_version_one_timing_boxes_and_multi_segment_edit() -> None:
    mdhd = b"\x01" + b"\x00" * 3 + b"\x00" * 8 + b"\x00" * 8
    mdhd += struct.pack(">IQ", 90000, 1080000) + struct.pack(">HH", 0x15C7, 0)
    assert _parse_mdhd(mdhd)[:2] == (90000, 1080000)
    mvhd = b"\x01" + b"\x00" * 3 + b"\x00" * 8 + b"\x00" * 8
    mvhd += struct.pack(">IQ", 1000, 12000)
    assert _parse_mvhd(mvhd) == (1000, 12000)
    elst = b"\x00\x00\x00\x00" + struct.pack(">I", 2)
    elst += struct.pack(">IiHH", 6000, 0, 1, 0) + struct.pack(">IiHH", 6000, 0, 1, 0)
    assert _parse_elst(elst, 1000) == (12000, None)
