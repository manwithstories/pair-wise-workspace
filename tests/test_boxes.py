from mkvtool.mp4.boxes import (
    avc1_sample_entry,
    box,
    build_stco,
    build_stss,
    build_stts,
    ftyp,
    mp4a_sample_entry,
    parse_boxes,
)


def test_box_round_trip_and_nested_child():
    child = box("abcd", b"12")
    parent = box("moov", child)
    parsed = parse_boxes(parent)
    assert parsed[0].type == b"moov"
    assert parsed[0].children()[0].type == b"abcd"
    assert parsed[0].children()[0].payload == b"12"


def test_sample_tables_and_entries_encode():
    stts = build_stts([40, 40, 40, 41])
    stss = build_stss([1, 5])
    stco = build_stco([100, 200])
    assert len(parse_boxes(stts)) == 1
    assert len(parse_boxes(stss)[0].payload) == 16
    assert len(parse_boxes(stco)[0].payload) == 16
    assert parse_boxes(ftyp())[0].type == b"ftyp"
    assert parse_boxes(avc1_sample_entry(1920, 1080, b"avcC"))[0].type == b"avc1"
    assert parse_boxes(mp4a_sample_entry(2, 48000, b"\x11\x90"))[0].type == b"mp4a"
