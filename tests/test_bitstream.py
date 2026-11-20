"""裸码流扫描与关键帧索引校验测试（夹具由代码拼装 Annex B 字节）。"""

import pytest

from mediasync.bitstream import (
    Codec,
    KeyframeEntry,
    build_timeline,
    scan_bitstream,
    validate_keyframe_index,
)
from mediasync.clock import MediaClock
from mediasync.errors import ErrorBook, ErrorCode


def _nal(nal_byte: int, body: bytes = b"\xaa\xbb", sc4: bool = False) -> bytes:
    sc = b"\x00\x00\x00\x01" if sc4 else b"\x00\x00\x01"
    return sc + bytes([nal_byte]) + body


def _h264_stream() -> bytes:
    # 7=SPS, 8=PPS, 5=IDR, 1=非 IDR；起始码 3/4 字节混用。
    return (
        _nal(0x67, sc4=True)
        + _nal(0x68)
        + _nal(0x65)
        + _nal(0x41)
        + _nal(0x41)
        + _nal(0x65)
    )


def test_scan_h264_offsets_types_and_keyframes():
    data = _h264_stream()
    book = ErrorBook()
    units = scan_bitstream(data, Codec.H264, source="v.h264", errors=book)

    assert [u.nal_type for u in units] == [7, 8, 5, 1, 1, 5]
    assert [u.is_keyframe for u in units] == [False, False, True, False, False, True]
    # 首个单元使用 4 字节起始码，偏移为 0；size 含起始码。
    assert units[0].offset == 0 and units[0].size == 1 + 4 + 2
    assert not book.any_fatal()


def test_scan_h265_cra_and_idr():
    data = _nal((21 << 1) & 0xFF) + _nal((1 << 1) & 0xFF) + _nal((19 << 1) & 0xFF)
    units = scan_bitstream(data, Codec.H265, source="v.h265")
    assert [u.nal_type for u in units] == [21, 1, 19]
    assert [u.is_keyframe for u in units] == [True, False, True]


def test_scan_av1_low_overhead_keyframe_detection():
    # OBU 头：type 在 bit3..6；frame header type=3 -> 0x1A；body 首字节高 2 位 00=KEY。
    frame_hdr = b"\x00\x00\x01" + bytes([0x1A]) + bytes([0x00, 0x12])
    metadata = b"\x00\x00\x01" + bytes([(2 << 3) | 0x02]) + b"\x01"
    inter_frame = b"\x00\x00\x01" + bytes([(6 << 3) | 0x02]) + bytes([0x80])
    units = scan_bitstream(frame_hdr + metadata + inter_frame, Codec.AV1, source="v.av1")
    assert [u.nal_type for u in units] == [3, 2, 6]
    assert [u.is_keyframe for u in units] == [True, False, False]


def test_scan_rejects_stream_without_start_code():
    book = ErrorBook()
    units = scan_bitstream(b"\x12\x34\x56", Codec.H264, source="bad.h264", errors=book)
    assert units == []
    assert book.has(ErrorCode.BITSTREAM_CORRUPT)
    assert book.any_fatal()


def test_scan_flags_forbidden_zero_bit():
    book = ErrorBook()
    scan_bitstream(_nal(0x85), Codec.H264, source="v.h264", errors=book)
    err = book.by_code(ErrorCode.BITSTREAM_CORRUPT)
    assert err and "禁止位" in err[0].message


def test_validate_index_rejects_out_of_range_duplicate_reversed():
    data = _h264_stream()
    book = ErrorBook()
    units = scan_bitstream(data, Codec.H264, errors=book)
    idr_offsets = [u.offset for u in units if u.is_keyframe]

    entries = [
        KeyframeEntry(idr_offsets[0], 0),
        KeyframeEntry(len(data) + 9, 1),  # 越界
        KeyframeEntry(idr_offsets[0], 2),  # 重复
        KeyframeEntry(idr_offsets[1] - 1, 3),  # 未指向关键帧
    ]
    accepted = validate_keyframe_index(entries, units, len(data), source="v.h264", errors=book)
    codes = {e.code for e in book.errors}
    assert ErrorCode.INDEX_OUT_OF_RANGE in codes
    assert ErrorCode.INDEX_DUPLICATE in codes
    assert ErrorCode.BITSTREAM_CORRUPT in codes
    assert accepted == [KeyframeEntry(idr_offsets[0], 0)]


def test_validate_index_reversed_offset_order():
    data = _h264_stream()
    book = ErrorBook()
    units = scan_bitstream(data, Codec.H264, errors=book)
    idr_offsets = [u.offset for u in units if u.is_keyframe]
    # 偏移严格倒序：第二项先触发倒序规则。
    entries = [KeyframeEntry(idr_offsets[1], 100), KeyframeEntry(idr_offsets[0], 50)]
    accepted = validate_keyframe_index(entries, units, len(data), errors=book)
    assert book.has(ErrorCode.INDEX_REVERSED)
    assert [e.offset for e in accepted] == [idr_offsets[1]]


def test_build_timeline_uses_shared_clock():
    clock = MediaClock(1000)
    book = ErrorBook()
    entries = [KeyframeEntry(0, 0), KeyframeEntry(12, 250), KeyframeEntry(40, 500)]
    timeline = build_timeline(entries, clock, source="v.h264", errors=book)
    assert [p.ms_rounded() for p in timeline.points] == [0, 250, 500]
    assert all(p.kind == "keyframe" for p in timeline.points)


@pytest.mark.parametrize("codec", [Codec.H264, Codec.H265, Codec.AV1])
def test_scan_smoke_all_three_codecs(codec):
    nal_byte = 0x65 if codec is Codec.H264 else ((19 << 1) & 0xFF)
    if codec is Codec.AV1:
        stream = b"\x00\x00\x01" + bytes([0x1A]) + b"\x00\x00"
    else:
        stream = _nal(nal_byte)
    units = scan_bitstream(stream, codec, source=f"v.{codec.value}")
    assert len(units) == 1
