import pytest

from mp4qc.boxio import BoxReader
from mp4qc.fixtures import _box, _stco, _ctts, _stsc, _stsd, _stsz, _stz2, _stts
from mp4qc.stbl import SampleTableError, parse_sample_table


def write_trak(tmp_path, stbl_payload):
    stbl = _box("stbl", stbl_payload)
    trak = _box("trak", _box("mdia", _box("minf", stbl)))
    path = tmp_path / "t.mp4"
    path.write_bytes(trak)
    with BoxReader(str(path)) as reader:
        box = next(reader.boxes())
        return parse_sample_table(reader, box, 1)


def test_stz2_field_sizes(tmp_path):
    for field_size, sizes in ((4, [1, 2, 3, 4, 5]),
                              (8, [10, 20, 250]),
                              (16, [1000, 60000])):
        payload = (_stsd("avc1") + _stts([100] * len(sizes))
                   + _stsc([(1, len(sizes))]) + _stz2(sizes, field_size)
                   + _stco([4096]))
        table = write_trak(tmp_path, payload)
        assert table.sizes == sizes
        assert table.sample_count == len(sizes)


def test_co64_chunk_offsets(tmp_path):
    offsets = [1 << 33, (1 << 33) + 500]
    payload = (_stsd("avc1") + _stts([100, 100]) + _stsc([(1, 1)])
               + _stsz([10, 10]) + _stco(offsets, co64=True))
    table = write_trak(tmp_path, payload)
    assert table.chunk_offsets == offsets


def test_stsc_run_overflow_reports_run_index(tmp_path):
    payload = (_stsd("avc1") + _stts([100] * 10) + _stsc([(1, 4)])
               + _stsz([10] * 10) + _stco([1000, 2000, 3000]))
    with pytest.raises(SampleTableError, match="stsc run 0"):
        write_trak(tmp_path, payload)


def test_ctts_v1_negative_offset_decode_order(tmp_path):
    offsets = [0, -100] + [0] * 8  # duplicate composition timestamps
    payload = (_stsd("avc1") + _stts([100] * 10) + _ctts(offsets, version=1)
               + _stsc([(1, 10)]) + _stsz([10] * 10) + _stco([1000]))
    table = write_trak(tmp_path, payload)
    assert table.ctts_offsets[1] == -100
    assert any("decode-order" in w for w in table.warnings)


def test_sample_ranges_walk_chunks(tmp_path):
    payload = (_stsd("avc1") + _stts([100] * 4) + _stsc([(1, 2)])
               + _stsz([10, 20, 30, 40]) + _stco([1000, 2000]))
    table = write_trak(tmp_path, payload)
    assert list(table.sample_ranges()) == [
        (1000, 10), (1010, 20), (2000, 30), (2030, 40)]

