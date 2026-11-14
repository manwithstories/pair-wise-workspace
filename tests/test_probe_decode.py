from __future__ import annotations

from framefix.decode import PixelSequence, iter_frame_records
from framefix.probe import iter_packet_meta, probe_file


def test_probe_reports_container_and_packet_metadata(bframes_path):
    info = probe_file(str(bframes_path))
    assert info.codec_name == "h264"
    assert info.width == 160 and info.height == 90
    packets = list(iter_packet_meta(str(bframes_path)))
    assert len(packets) == 72
    assert packets[0].is_keyframe is True
    assert packets[0].time_base == info.time_base
    assert any(packet.pts is not None and packet.dts is not None for packet in packets)


def test_decode_is_full_and_pixel_summaries_are_stable(bframes_path):
    first = PixelSequence()
    records = []
    for record in iter_frame_records(str(bframes_path)):
        first.update(record)
        records.append(record)
    second = PixelSequence()
    for record in iter_frame_records(str(bframes_path)):
        second.update(record)

    assert len(records) == 72
    assert records[0].pixel_digest
    assert len(records[0].pixel_digest) == 32
    assert first.hexdigest == second.hexdigest
    assert {record.pict_type for record in records} <= {"I", "P", "B", "?"}
    assert all(record.packet_seq is not None for record in records)

