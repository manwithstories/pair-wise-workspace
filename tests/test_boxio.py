import struct

import pytest

from mp4qc.boxio import BoxReader, TruncatedError, parse_header


def make_box(type4, payload):
    return struct.pack(">I4s", 8 + len(payload), type4) + payload


def test_plain_box_offsets():
    data = make_box(b"ftyp", b"\x00" * 16) + make_box(b"moov", b"\x01" * 8)
    boxes = list(parse_header(data, 0, len(data)) for _ in range(1))
    assert boxes[0].type == "ftyp"
    assert boxes[0].start == 0
    assert boxes[0].size == 24
    assert boxes[0].data_offset == 8


def test_largesize_64bit():
    payload = b"\x00" * 32
    data = struct.pack(">I4sQ", 1, b"mdat", 16 + len(payload)) + payload
    box = parse_header(data, 0, len(data))
    assert box.type == "mdat"
    assert box.header_size == 16
    assert box.size == 48


def test_size_zero_extends_to_end():
    head = make_box(b"ftyp", b"\x00" * 8)
    tail = struct.pack(">I4s", 0, b"mdat") + b"\x00" * 40
    data = head + tail
    box = parse_header(data, len(head), len(data))
    assert box.size == len(tail)
    assert box.end == len(data)


def test_uuid_usertype():
    usertype = bytes(range(16))
    data = struct.pack(">I4s", 8 + 16 + 4, b"uuid") + usertype + b"\x00" * 4
    box = parse_header(data, 0, len(data))
    assert box.type == "uuid"
    assert box.usertype == usertype
    assert box.header_size == 24


def test_truncation_reports_byte_position():
    data = struct.pack(">I4s", 1000, b"mdat") + b"\x00" * 10
    with pytest.raises(TruncatedError) as exc:
        parse_header(data, 0, len(data))
    assert exc.value.offset == 0
    assert exc.value.box_type == "mdat"
    assert exc.value.needed == 1000


def test_reader_walks_top_level(tmp_path):
    payload = make_box(b"ftyp", b"\x00" * 8) + make_box(b"free", b"\x00" * 4)
    path = tmp_path / "a.mp4"
    path.write_bytes(payload)
    with BoxReader(str(path)) as reader:
        types = [b.type for b in reader.boxes()]
        assert types == ["ftyp", "free"]
        assert reader.size == len(payload)

