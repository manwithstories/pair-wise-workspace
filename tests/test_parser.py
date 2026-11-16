from __future__ import annotations

import struct
import time
from pathlib import Path

import pytest

from sidxcheck.boxes import parse_bytes
from sidxcheck.fixtures import (
    SAMPLE_DURATION,
    SAMPLE_SIZE,
    SAMPLES_PER_SEGMENT,
    SEGMENT_SECONDS,
    TIMESCALE,
    generate_fixture,
)
from sidxcheck.sidx import SidxEntry, SidxError, encode_sidx, parse_sidx
from sidxcheck.stbl import parse_stbl
from sidxcheck.verify import exit_code, scan_path, verify_scan


def box(box_type: bytes, payload: bytes = b"") -> bytes:
    return struct.pack(">I4s", len(payload) + 8, box_type) + payload


def full_box(box_type: bytes, version: int, flags: int, payload: bytes) -> bytes:
    return box(box_type, struct.pack(">I", (version << 24) | flags) + payload)


def test_box_sizes_uuid_unknown_and_size_zero() -> None:
    uuid_user_type = b"0123456789abcdef"
    raw = b"".join(
        [
            box(b"ftyp", b"iso6\x00\x00\x00\x01iso6"),
            struct.pack(">I4sQ", 1, b"wide", 16),
            box(b"uuid", uuid_user_type + b"payload"),
            box(b"zzzz", b"skip me"),
            struct.pack(">I4s", 0, b"mdat") + b"tail",
        ]
    )
    boxes, issues = parse_bytes(raw)
    assert not issues
    assert [item.type for item in boxes] == ["ftyp", "wide", "uuid", "zzzz", "mdat"]
    assert boxes[1].header_size == 16
    assert boxes[2].uuid == uuid_user_type
    assert boxes[-1].end == len(raw)


def test_truncated_top_level_box_resynchronises() -> None:
    bad_moof = struct.pack(">I4s", 4, b"moof")
    raw = box(b"free") + bad_moof + box(b"moof")
    boxes, issues = parse_bytes(raw)
    assert [item.type for item in boxes] == ["free", "moof"]
    assert issues and issues[0].code == "invalid_size"


def test_depth_and_box_size_limits() -> None:
    nested = box(b"moov", box(b"trak", box(b"mdia", b"")))
    boxes, issues = parse_bytes(nested, max_depth=1)
    assert boxes[0].type == "moov"
    assert any(issue.code == "max_depth" for issue in issues)

    boxes, issues = parse_bytes(box(b"free", bytes(32)), max_box_size=16)
    assert boxes == []
    assert issues[0].code == "box_size_limit"


def test_sidx_versions_hierarchy_and_count_mismatch() -> None:
    entries = [
        SidxEntry(0, 0, 100, 500),
        SidxEntry(1, 1, 50, 999),
    ]
    raw = encode_sidx(
        reference_id=7,
        timescale=90_000,
        earliest_presentation_time=1 << 33,
        first_offset=1 << 32,
        entries=entries,
    )
    boxes, issues = parse_bytes(raw)
    assert not issues
    parsed = parse_sidx(boxes[0], raw)
    assert parsed.version == 1
    assert parsed.reference_id == 7
    assert parsed.timescale == 90_000
    assert parsed.earliest_presentation_time == 1 << 33
    assert parsed.first_offset == 1 << 32
    assert len(parsed.entries) == 2
    assert len(parsed.sample_entries) == 1
    assert parsed.total_duration == 500
    assert parsed.entries[1].is_hierarchical

    short = encode_sidx(
        reference_id=1,
        timescale=1000,
        earliest_presentation_time=0,
        first_offset=0,
        entries=entries,
        declared_reference_count=1,
    )
    boxes, _ = parse_bytes(short)
    parsed = parse_sidx(boxes[0], short)
    assert len(parsed.entries) == 1
    assert parsed.declared_reference_count == 1
    assert parsed.issues


def test_sidx_rejects_truncated_payload_and_v0_overflow() -> None:
    raw = encode_sidx(
        reference_id=1,
        timescale=1000,
        earliest_presentation_time=0,
        first_offset=0,
        entries=[SidxEntry(0, 0, 1, 1)],
    )
    boxes, _ = parse_bytes(raw)
    with pytest.raises(SidxError):
        parse_sidx(boxes[0], raw[:-4])
    with pytest.raises(SidxError):
        encode_sidx(
            reference_id=1,
            timescale=1000,
            earliest_presentation_time=0,
            first_offset=1 << 32,
            entries=[],
            version=0,
        )


def test_moof_and_sample_table_mapping(tmp_path: Path) -> None:
    target = tmp_path / "normal"
    generate_fixture(target, segments=3, writer="normal")
    scan = scan_path(target)
    assert len(scan.moofs) == 3
    assert [item.tfdt for item in scan.moofs] == [0, 512_000, 1_024_000]
    assert all(item.duration_ticks == SEGMENT_SECONDS * TIMESCALE for item in scan.moofs)
    assert scan.moofs[0].sample_count == SAMPLES_PER_SEGMENT

    table = scan.sample_tables[0]
    assert table.track_id == 1
    assert table.timescale == TIMESCALE
    assert table.sample_count == 3 * SAMPLES_PER_SEGMENT
    assert len(table.sample_locations) == table.sample_count
    assert table.sample_locations[0].offset == scan.moofs[0].data_start
    assert table.sample_locations[0].size == SAMPLE_SIZE
    assert table.sample_locations[0].duration == SAMPLE_DURATION
    assert exit_code(verify_scan(scan)) == 0


def test_truncated_fixture_records_fatal_and_continues(tmp_path: Path) -> None:
    target = tmp_path / "truncated"
    generate_fixture(target, segments=8, writer="truncated")
    scan = scan_path(target)
    assert scan.boundary_count == 8
    assert scan.fatal_count == 1
    assert scan.fatal[0].code == "invalid_size"
    result = verify_scan(scan)
    assert exit_code(result) == 3
    assert result.offset_mismatch == 0


def test_co64_above_four_gibibytes() -> None:
    stts = full_box(b"stts", 0, 0, struct.pack(">III", 1, 1, 100))
    stsc = full_box(b"stsc", 0, 0, struct.pack(">IIII", 1, 1, 1, 1))
    stsz = full_box(b"stsz", 0, 0, struct.pack(">III", 0, 1, 4))
    co64 = full_box(b"co64", 0, 0, struct.pack(">IQ", 1, 1 << 32))
    raw = box(b"stbl", stts + stsc + stsz + co64)
    boxes, issues = parse_bytes(raw)
    assert not issues
    table = parse_stbl(boxes[0], raw, file=Path("co64.mp4"), timescale=1000)
    assert table.chunk_offsets == [1 << 32]
    assert table.sample_locations[0].offset == 1 << 32
    assert not table.issues


def test_empty_single_sample_and_zero_duration_tables() -> None:
    empty = box(b"stbl")
    boxes, _ = parse_bytes(empty)
    table = parse_stbl(boxes[0], empty, file=Path("empty.mp4"), timescale=1000)
    assert table.sample_count == 0
    assert table.total_duration == 0
    assert not table.issues

    stts = full_box(b"stts", 0, 0, struct.pack(">III", 1, 1, 0))
    stsc = full_box(b"stsc", 0, 0, struct.pack(">IIII", 1, 1, 1, 1))
    stsz = full_box(b"stsz", 0, 0, struct.pack(">III", 0, 1, 0))
    stco = full_box(b"stco", 0, 0, struct.pack(">II", 1, 500))
    raw = box(b"stbl", stts + stsc + stsz + stco)
    boxes, _ = parse_bytes(raw)
    table = parse_stbl(boxes[0], raw, file=Path("single.mp4"), timescale=1000)
    assert table.sample_count == 1
    assert table.total_duration == 0
    assert table.sample_locations[0].offset == 500
    assert not table.issues


def test_stsz_stsc_stco_validation() -> None:
    stts = full_box(b"stts", 0, 0, struct.pack(">III", 1, 1, 100))
    stsc = full_box(b"stsc", 0, 0, struct.pack(">IIII", 1, 1, 2, 1))
    stsz = full_box(b"stsz", 0, 0, struct.pack(">II", 1, 2))
    stco = full_box(b"stco", 0, 0, struct.pack(">III", 2, 1000, 900))
    raw = box(b"stbl", stts + stsc + stsz + stco)
    boxes, _ = parse_bytes(raw)
    table = parse_stbl(boxes[0], raw, file=Path("bad.mp4"), timescale=1000)
    assert any("stts covers" in issue for issue in table.issues)
    assert any("not monotonic" in issue for issue in table.issues)
    assert any("stsc maps" in issue for issue in table.issues)


def test_hundred_thousand_samples_under_five_seconds() -> None:
    count = 100_000
    stts = full_box(b"stts", 0, 0, struct.pack(">III", 1, count, 1))
    stsc = full_box(b"stsc", 0, 0, struct.pack(">IIII", 1, 1, count, 1))
    stsz = full_box(b"stsz", 0, 0, struct.pack(">II", 1, count))
    stco = full_box(b"stco", 0, 0, struct.pack(">II", 1, 1000))
    raw = box(b"stbl", stts + stsc + stsz + stco)
    boxes, _ = parse_bytes(raw)
    started = time.perf_counter()
    table = parse_stbl(boxes[0], raw, file=Path("large.mp4"), timescale=1000)
    elapsed = time.perf_counter() - started
    assert len(table.sample_locations) == count
    assert table.sample_locations[-1].offset == 1000 + count - 1
    assert elapsed < 5.0
    assert not table.issues
