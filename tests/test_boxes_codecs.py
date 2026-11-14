from __future__ import annotations

import io
import struct

import pytest

from mediaprobe.boxes import find_box, parse_boxes
from mediaprobe.codecs import parse_audio_specific_config, parse_avcc, parse_esds, parse_hvcc
from mediaprobe.errors import MediaError
from mediaprobe.samplegen import box


def test_mixed_box_sizes_uuid_and_nesting() -> None:
    avcc = box("avcC", bytes([1, 100, 0, 40]))
    trak = box("trak", box("mdia", box("minf", box("stbl", avcc))))
    large = box("mdat", b"0123456789", large=True)
    uuid = box("uuid", b"mediaprobe-uuid0" + b"ignored")
    zero = box("free", b"tail", zero=True)
    payload = box("ftyp", b"isom") + large + uuid + box("moov", trak) + zero
    roots = parse_boxes(io.BytesIO(payload))
    assert [item.type for item in roots] == ["ftyp", "mdat", "uuid", "moov", "free"]
    assert roots[1].header_size == 16
    assert roots[1].data_size == 10
    assert roots[2].header_size == 24
    assert roots[4].size == 12
    avcc_box = find_box(roots, "avcC")
    assert avcc_box is not None
    assert avcc_box.path.endswith("stbl[0]/avcC[0]")
    assert avcc_box.end - avcc_box.offset == len(avcc)


def test_malformed_size_reports_offset() -> None:
    data = struct.pack(">I", 4) + b"free" + b"junk"
    with pytest.raises(MediaError, match="BOX_MALFORMED") as caught:
        parse_boxes(io.BytesIO(data))
    assert caught.value.offset == 0


def test_truncated_box_is_distinct_error() -> None:
    data = struct.pack(">I", 64) + b"mdat" + b"abc"
    with pytest.raises(MediaError) as caught:
        parse_boxes(io.BytesIO(data))
    assert caught.value.code == "FILE_TRUNCATED"
    assert caught.value.offset == 0


def test_large_uuid_header_places_user_type_after_64bit_size() -> None:
    user_type = b"mediaprobe-uuid0"
    payload = struct.pack(">I", 1) + b"uuid" + struct.pack(">Q", 33) + user_type + b"x"
    box_item = parse_boxes(io.BytesIO(payload))[0]
    assert box_item.header_size == 32
    assert box_item.data_size == 1
    assert box_item.user_type == user_type


def test_avcc_and_hvcc_profiles() -> None:
    avc = parse_avcc(bytes([1, 100, 0, 40]))
    assert (avc.profile, avc.level, avc.codec) == ("High", "4.0", "h264")
    hevc = parse_hvcc(bytes([1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 120]))
    assert (hevc.profile, hevc.level, hevc.codec) == ("Main", "4.0", "hevc")


def test_esds_extracts_aac_rate_and_channels() -> None:
    asc = bytes([0x11, 0x90])
    decoder = bytes([0x40, 0x05, 0, 0, 0]) + b"\x00" * 12 + bytes([5, len(asc)]) + asc
    es = b"\x00\x01\x00" + bytes([4, len(decoder)]) + decoder + bytes([6, 2, 0x02])
    esds = bytes([0, 0, 0, 0, 3, len(es)]) + es
    info = parse_esds(esds)
    assert (info.codec, info.sample_rate, info.channels) == ("aac", 48000, 2)
    assert parse_audio_specific_config(asc) == (2, 48000, 2)
