from mp4qc.boxio import BoxReader
from mp4qc.fixtures import _box, _build_moof, _fullbox
from mp4qc.frag import parse_moof


def load_moof(tmp_path, data):
    path = tmp_path / "f.mp4"
    path.write_bytes(data)
    with BoxReader(str(path)) as reader:
        box = next(reader.boxes())
        return parse_moof(reader, box), box


def test_moof_defaults_and_signed_data_offset(tmp_path):
    sizes = [100, 200, 300]
    data = _build_moof(7, 1, 90000, sizes, 1024)
    moof, box = load_moof(tmp_path, data)
    assert moof.sequence_number == 7
    traf = moof.trafs[0]
    assert traf.track_id == 1
    assert traf.default_base_is_moof is True
    assert traf.default_sample_duration == 1024
    assert traf.base_media_decode_time == 90000
    trun = traf.truns[0]
    assert trun.data_offset == box.size + 8
    assert [s.size for s in trun.samples] == sizes
    start, length = traf.data_extent(moof.start)
    assert start == box.size + 8
    assert length == 600


def test_multi_traf_grouped_by_track(tmp_path):
    import struct
    mfhd = _fullbox("mfhd", 0, 0, struct.pack(">I", 1))
    traf1 = _box("traf", _fullbox("tfhd", 0, 0x020000, struct.pack(">I", 1)))
    traf2 = _box("traf", _fullbox("tfhd", 0, 0x020000, struct.pack(">I", 2)))
    moof, _box_ = load_moof(tmp_path, _box("moof", mfhd + traf1 + traf2))
    grouped = moof.by_track()
    assert sorted(grouped) == [1, 2]

