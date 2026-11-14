from __future__ import annotations

from fractions import Fraction

from framefix.models import FrameRecord, MediaInfo, PacketMeta
from framefix.repair import TimestampAnalyzer


def _info() -> MediaInfo:
    return MediaInfo(
        path="memory.mp4",
        format_name="mp4",
        stream_index=0,
        codec_name="h264",
        time_base=Fraction(1, 1000),
        width=64,
        height=64,
        average_rate=Fraction(25),
        duration=Fraction(4, 25),
    )


def test_timestamp_analyzer_covers_all_repair_reasons():
    analyzer = TimestampAnalyzer(_info(), max_jump_ms=250, capture_mapping=True)
    dts_values = [10, 10, 0, 400]
    for index in range(4):
        analyzer.add_frame(
            FrameRecord(
                index=index,
                pts=[0, 40, 80, 0][index],
                dts=dts_values[index],
                time_base=Fraction(1, 1000),
                pict_type="P",
                width=64,
                height=64,
                pixel_digest=f"{index:032x}",
                luma_mean=0.0,
                packet_seq=index,
                packet_keyframe=index == 0,
            )
        )
        analyzer.add_packet(
            PacketMeta(
                seq=index,
                stream_index=0,
                pts=[0, 40, 80, 0][index],
                dts=dts_values[index],
                duration=40,
                time_base=Fraction(1, 1000),
                is_keyframe=index == 0,
                is_discard=False,
                size=10,
            )
        )
    plan = analyzer.finish()
    for reason in (
        "pts_rollback",
        "dts_duplicate",
        "negative_interval",
        "jump",
        "first_dts_nonzero",
    ):
        assert plan.reasons[reason] >= 1
    assert plan.fixed_entries == len(plan.entries)
    assert plan.packet_dts[0] == 0
    assert plan.packet_dts[1] == 40
    assert plan.max_jump_ms >= 400

