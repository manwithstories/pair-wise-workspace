from __future__ import annotations

from fractions import Fraction

from framefix.models import PacketMeta
from framefix.reorder import ReorderBuffer
from framefix.report import analyze_media


def test_decoded_types_cross_check_packet_flags(bframes_path):
    result = analyze_media(str(bframes_path))
    classifier = result.classifier
    assert result.status_code == 0
    assert classifier.i_frames > 0
    assert classifier.p_frames > 0
    assert classifier.b_frames > 0
    assert classifier.type_mismatch == 0
    assert classifier.gop_count == len(classifier.gops)
    assert sum(gop.length for gop in classifier.gops) == result.plan.decoded_frames


def test_reorder_buffer_is_bounded_and_detects_dts_faults():
    buffer = ReorderBuffer(window=3)
    rows = [(0, 0), (2, 2), (2, 3), (1, 1)]
    emitted = []
    for seq, (dts, pts) in enumerate(rows):
        packet = PacketMeta(
            seq=seq,
            stream_index=0,
            pts=pts,
            dts=dts,
            duration=1,
            time_base=Fraction(1, 10),
            is_keyframe=False,
            is_discard=False,
            size=1,
        )
        emitted.extend(buffer.push(packet))
    ordered = emitted + list(buffer.flush())
    assert [packet.pts for packet in ordered] == [0, 1, 2, 3]
    assert buffer.summary.duplicate_dts == 1
    assert buffer.summary.regressed_dts == 1
    assert buffer.summary.max_buffer_depth <= 3

