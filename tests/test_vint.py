import pytest

from mkvtool.ebml.vint import (
    encode_vint,
    encode_unknown_size,
    parse_element_header,
    read_vint,
    vint_length,
)
from mkvtool.errors import InputParseError


@pytest.mark.parametrize("width", range(1, 9))
def test_vint_round_trip_all_widths(width):
    value = (1 << (7 * width - 1)) - 1
    encoded = encode_vint(value, length=width)
    assert len(encoded) == width
    assert vint_length(encoded[0]) == width
    assert read_vint(encoded, 0)[:2] == (value, width)


def test_unknown_size_marker():
    for width in range(1, 9):
        value, actual_width, unknown = read_vint(encode_unknown_size(width), 0)
        assert actual_width == width
        assert unknown
        assert value == (1 << (7 * width)) - 1


def test_element_header_with_unknown_size():
    data = bytes.fromhex("1f43b675ff")
    header = parse_element_header(data, 0)
    assert header.element_id == 0x1F43B675
    assert header.unknown_size
    assert header.header_length == 5


def test_invalid_and_truncated_vint():
    with pytest.raises(InputParseError):
        read_vint(b"\x00", 0)
    with pytest.raises(InputParseError):
        read_vint(b"\x40", 0)
    with pytest.raises(InputParseError):
        read_vint(b"\x01", 0)
