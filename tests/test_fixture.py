"""自生成夹具测试：真实 PyAV 编码、关键帧索引与损坏注入定位精度。"""

import pytest

from mediasync.bitstream import (
    Codec,
    KeyframeEntry,
    _find_start_codes,
    scan_bitstream,
)
from mediasync.errors import ErrorBook, ErrorCode
from mediasync.fixture import (
    RepresentationSpec,
    build_keyframe_index,
    encode_video,
    inject_corrupt_header,
    inject_index_offset,
    inject_timestamp_backtrack,
    inject_truncated,
)


@pytest.mark.parametrize(
    "codec",
    [Codec.H264, Codec.H265, Codec.AV1],
)
def test_encode_annexb_has_start_codes_and_keyframes(codec):
    spec = RepresentationSpec(name="rep", codec=codec, frames=30, gop=15)
    data = encode_video(spec)

    marks = _find_start_codes(data)
    assert marks and marks[0][0] == 0  # 以起始码开头
    book = ErrorBook()
    units = scan_bitstream(data, codec, source="rep", errors=book)
    assert not book.any_fatal()
    key_offsets = [u.offset for u in units if u.is_keyframe]
    # GOP=15、30 帧：至少 0ms 与 500ms 两个关键帧；首个单元通常是 SPS/PPS。
    assert len(key_offsets) >= 2
    assert all(off > 0 or off == 0 for off in key_offsets)
    # 关键帧偏移严格递增且都落在文件内。
    assert key_offsets == sorted(set(key_offsets))
    assert key_offsets[-1] < len(data)


def test_keyframe_index_ticks_match_500ms_grid():
    spec = RepresentationSpec(name="rep", codec=Codec.H264, timescale=30, fps=30, gop=15)
    data = encode_video(spec)
    entries = build_keyframe_index(data, spec.codec, spec.timescale, spec.fps, spec.gop)

    # tick 15 -> 500ms（timescale=30）。
    assert entries[0].tick == 0
    assert entries[1].tick == 15
    assert all(e.offset >= 0 for e in entries)


def test_inject_corrupt_header_offset_within_one_byte_and_detected():
    spec = RepresentationSpec(name="rep", codec=Codec.H264)
    data = encode_video(spec)
    mutated, offset = inject_corrupt_header(data, nth=7)

    assert abs(mutated[offset] & 0x80) == 0x80
    book = ErrorBook()
    scan_bitstream(mutated, Codec.H264, source="rep", errors=book)
    corrupt = book.by_code(ErrorCode.BITSTREAM_CORRUPT)
    assert corrupt and corrupt[0].offset == offset


def test_inject_corrupt_header_works_for_av1_forbidden_bit():
    spec = RepresentationSpec(name="rep", codec=Codec.AV1)
    data = encode_video(spec)
    mutated, offset = inject_corrupt_header(data, nth=3)
    book = ErrorBook()
    scan_bitstream(mutated, Codec.AV1, source="rep", errors=book)
    assert any(e.offset == offset for e in book.by_code(ErrorCode.BITSTREAM_CORRUPT))


def test_inject_truncated_cuts_at_start_code_boundary():
    spec = RepresentationSpec(name="rep", codec=Codec.H265)
    data = encode_video(spec)
    cut_data, cut_offset = inject_truncated(data)

    assert len(cut_data) == cut_offset
    # 截断后起始码数量减少，且剩余部分仍可扫描。
    assert len(_find_start_codes(cut_data)) < len(_find_start_codes(data))


def test_inject_index_offset_points_away_from_keyframe():
    spec = RepresentationSpec(name="rep", codec=Codec.H264)
    data = encode_video(spec)
    entries = build_keyframe_index(data, spec.codec, 30, 30, 15)
    broken = inject_index_offset(entries, shift=5)

    book = ErrorBook()
    units = scan_bitstream(data, Codec.H264, errors=book)
    from mediasync.bitstream import validate_keyframe_index

    validate_keyframe_index(broken, units, len(data), errors=book)
    assert book.has(ErrorCode.BITSTREAM_CORRUPT)


def test_inject_timestamp_backtrack_preserves_offsets_breaks_ticks():
    entries = [KeyframeEntry(0, 0), KeyframeEntry(100, 15), KeyframeEntry(200, 30)]
    broken = inject_timestamp_backtrack(entries)

    assert [e.offset for e in broken] == [0, 100, 200]  # 偏移仍递增
    assert broken[-1].tick < broken[-2].tick  # tick 回退
