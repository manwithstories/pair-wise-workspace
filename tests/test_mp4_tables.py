from __future__ import annotations

import struct

from mkvmp4.mp4.boxes import BoxBuffer, box
from mkvmp4.mp4.stbl import SampleMeta, ctts_box, stsc_box
from mkvmp4.remux import RawSample, RepairResult, TrackPlan, _edts, _layout_samples, _repair_track


def test_box_buffer_backfills_inner_and_outer_sizes() -> None:
    out = BoxBuffer()
    out.start("moov")
    out.start("trak")
    out.start("tkhd", version=0, flags=3)
    out.write(b"abc")
    out.end()
    out.end()
    out.end()
    data = out.getvalue()
    outer, inner = struct.unpack(">I4s", data[:8])[0], struct.unpack(">I4s", data[8:16])[0]
    assert outer == len(data)
    assert inner == len(data) - 8
    assert struct.unpack(">I4s", data[16:24]) == (8 + 4 + 3, b"tkhd")


def test_ctts_uses_signed_version_for_negative_offsets() -> None:
    samples = [
        SampleMeta(0, 1, 0, 0, 10, True, 0),
        SampleMeta(0, 1, 10, 0, 10, False, 0),
        SampleMeta(0, 1, 20, 10, 10, False, 0),
    ]
    data = ctts_box(samples)
    assert data is not None
    assert data[8] == 1
    count = struct.unpack(">I", data[12:16])[0]
    assert count == 2
    assert struct.unpack(">i", data[28:32])[0] == -10


def test_stsc_groups_one_chunk_per_cluster() -> None:
    samples = [SampleMeta(0, 1, i, i, 1, True, cluster)
               for i, cluster in enumerate([0, 0, 1, 1])]
    data = stsc_box(samples)
    count = struct.unpack(">I", data[12:16])[0]
    assert count == 1
    first, per_chunk, desc = struct.unpack(">III", data[16:28])
    assert (first, per_chunk, desc) == (1, 2, 1)


def test_b_frame_reorder_produces_ctts_and_elst() -> None:
    plan = TrackPlan(object(), 1, "avc1", "vide")
    plan.samples = [
        RawSample(1, 0, 0, 0, 0, 40_000, True, False, 0, 1),
        RawSample(1, 0, 1, 80_000, 80_000, 40_000, False, False, 0, 1),
        RawSample(1, 0, 2, 80_000, 40_000, 40_000, False, True, 0, 1),
        RawSample(1, 0, 3, 120_000, 120_000, 40_000, False, False, 0, 1),
    ]
    result = RepairResult()
    _repair_track(plan, "clamp", result)
    assert result.repaired == 0
    _layout_samples({1: plan}, 1)
    offsets = [meta.cts_offset for meta in plan.sample_meta]
    assert offsets == [0, 40, -40, 0]
    assert plan.edit_media_time == 40
    assert b"elst" in _edts(plan)

