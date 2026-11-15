"""Tests for stbl sample table expansion."""
import io
import struct

import pytest

from isobmff.boxes import build_tree
from isobmff.fixtures import FRAME_DURATION, FixtureParams, _box, _fullbox, generate
from isobmff.reader import ByteCursor, StructureError
from isobmff.stbl import expand_samples, parse_ctts, parse_stsc, parse_stsz


def cursor_of(data: bytes) -> ByteCursor:
    return ByteCursor(io.BytesIO(data), len(data))


def test_stts_accumulates_dts_and_pts():
    blob = generate(FixtureParams(samples=24, fragments=2, seed=3))
    cursor = cursor_of(blob)
    root = build_tree(cursor)
    stbl = root.find_one("moov/trak/mdia/minf/stbl")
    samples = expand_samples(cursor, stbl)
    assert len(samples) == 24
    for i, sample in enumerate(samples):
        assert sample.dts == i * FRAME_DURATION
    # ctts pattern from fixtures: ((i // 8) % 3 - 1) * 512
    assert samples[0].pts == samples[0].dts - 512
    assert samples[8].pts == samples[8].dts + 0
    assert samples[16].pts == samples[16].dts + 512


def test_ctts_signed_offsets_version1():
    payload = struct.pack(">IIiIi", 2, 3, -100, 2, 250)
    cursor = cursor_of(_fullbox(b"ctts", 1, 0, payload))
    box = build_tree(cursor).children[0]
    assert parse_ctts(cursor, box) == [-100, -100, -100, 250, 250]


def test_stsc_first_chunk_must_increase():
    payload = struct.pack(">I", 2) + struct.pack(">III", 2, 1, 1)
    payload += struct.pack(">III", 1, 1, 1)  # decreasing first_chunk
    cursor = cursor_of(_fullbox(b"stsc", 0, 0, payload))
    box = build_tree(cursor).children[0]
    with pytest.raises(StructureError):
        parse_stsc(cursor, box)


def test_stsz_constant_and_variable():
    const = _fullbox(b"stsz", 0, 0, struct.pack(">II", 99, 4))
    cursor = cursor_of(const)
    box = build_tree(cursor).children[0]
    assert parse_stsz(cursor, box) == [99, 99, 99, 99]
    var = _fullbox(b"stsz", 0, 0, struct.pack(">IIII", 0, 2, 7, 8))
    cursor = cursor_of(var)
    box = build_tree(cursor).children[0]
    assert parse_stsz(cursor, box) == [7, 8]


def test_co64_chunk_offsets():
    stts = _fullbox(b"stts", 0, 0, struct.pack(">III", 1, 2, 100))
    stsc = _fullbox(b"stsc", 0, 0, struct.pack(">IIII", 1, 1, 2, 1))
    stsz = _fullbox(b"stsz", 0, 0, struct.pack(">IIII", 0, 2, 10, 20))
    co64 = _fullbox(b"co64", 0, 0, struct.pack(">IQ", 1, 1 << 40))
    stbl_bytes = _box(b"stbl", stts + stsc + stsz + co64)
    cursor = cursor_of(stbl_bytes)
    stbl = build_tree(cursor).children[0]
    samples = expand_samples(cursor, stbl)
    assert [s.offset for s in samples] == [1 << 40, (1 << 40) + 10]


def test_stsc_chunk_out_of_range():
    stts = _fullbox(b"stts", 0, 0, struct.pack(">III", 1, 2, 100))
    stsc = _fullbox(b"stsc", 0, 0, struct.pack(">IIII", 1, 5, 2, 1))
    stsz = _fullbox(b"stsz", 0, 0, struct.pack(">IIII", 0, 2, 10, 20))
    stco = _fullbox(b"stco", 0, 0, struct.pack(">II", 1, 0))
    stbl_bytes = _box(b"stbl", stts + stsc + stsz + stco)
    cursor = cursor_of(stbl_bytes)
    stbl = build_tree(cursor).children[0]
    with pytest.raises(StructureError, match="exceeds chunk count"):
        expand_samples(cursor, stbl)
