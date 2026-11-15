"""Tests for sidx parsing and hierarchical reference expansion."""
import io

from isobmff.boxes import build_tree
from isobmff.fixtures import build_sidx
from isobmff.reader import ByteCursor
from isobmff.sidx import cumulative_durations, expand_sidx, parse_sidx


def cursor_of(data: bytes) -> ByteCursor:
    return ByteCursor(io.BytesIO(data), len(data))


def parse_first(data: bytes):
    cursor = cursor_of(data)
    box = build_tree(cursor).children[0]
    return cursor, parse_sidx(cursor, box)


def test_parse_version0():
    blob = build_sidx(0, [(0, 100, 900), (0, 200, 1800)], first_offset=4)
    cursor, sidx = parse_first(blob)
    assert sidx.version == 0
    assert sidx.first_offset == 4
    assert len(sidx.references) == 2
    assert sidx.references[1].subsegment_duration == 1800


def test_parse_version1_byte_ranges():
    media = b"\0" * 300
    blob = build_sidx(1, [(0, 100, 900), (0, 200, 1800)]) + media
    cursor, sidx = parse_first(blob)
    leaves = expand_sidx(cursor, sidx)
    assert len(leaves) == 2
    assert leaves[0].byte_start == len(blob) - 300
    assert leaves[0].byte_end == leaves[0].byte_start + 100
    assert leaves[1].byte_start == leaves[0].byte_end
    assert cumulative_durations(leaves) == [900, 2700]


def test_hierarchical_reference_recursion():
    media = b"\0" * 150
    inner = build_sidx(1, [(0, 50, 300), (0, 100, 600)])
    outer = build_sidx(1, [(1, len(inner), 900)])
    blob = outer + inner + media
    cursor, sidx = parse_first(blob)
    leaves = expand_sidx(cursor, sidx)
    assert len(leaves) == 2
    base = len(outer) + len(inner)
    assert leaves[0].byte_start == base
    assert leaves[1].byte_start == base + 50
    assert [r.subsegment_duration for r in leaves] == [300, 600]
