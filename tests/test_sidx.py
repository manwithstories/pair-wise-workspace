import struct

from mp4qc.boxio import BoxReader
from mp4qc.sidx import (Sidx, SidxReference, build_sidx, parse_sidx,
                        reference_spans)


def roundtrip(tmp_path, sidx):
    data = build_sidx(sidx)
    path = tmp_path / "s.mp4"
    path.write_bytes(data)
    with BoxReader(str(path)) as reader:
        box = next(reader.boxes())
        assert box.type == "sidx"
        return parse_sidx(reader, box), box


def test_roundtrip_v0_sap_bits(tmp_path):
    refs = [SidxReference(0, 1000, 40960, True, 2, 5),
            SidxReference(0, 2000, 40960, False, 0, 0)]
    parsed, box = roundtrip(tmp_path, Sidx(0, 1, 90000, 0, 0, refs))
    assert parsed.version == 0
    assert parsed.references[0].sap_type == 2
    assert parsed.references[0].sap_delta_time == 5
    assert parsed.references[1].starts_with_sap is False
    spans = reference_spans(parsed, box.end)
    assert [s[0] for s in spans] == [box.end, box.end + 1000]


def test_version_bump_on_32bit_overflow(tmp_path):
    refs = [SidxReference(0, 100, 10)]
    sidx = Sidx(0, 1, 90000, 1 << 33, 0, refs)  # ept overflows 32 bits
    parsed, _box = roundtrip(tmp_path, sidx)
    assert parsed.version == 1
    assert parsed.earliest_presentation_time == 1 << 33


def test_reference_type_bit(tmp_path):
    refs = [SidxReference(1, 500, 0)]
    parsed, _box = roundtrip(tmp_path, Sidx(0, 1, 90000, 0, 0, refs))
    assert parsed.references[0].reference_type == 1
    assert parsed.references[0].referenced_size == 500

