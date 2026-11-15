"""Unit coverage for VINT primitives and box serializers."""

import pytest

from mkvkit.ebml.vint import (
    VintInvalid,
    VintTruncated,
    decode_signed,
    encode_id,
    encode_size,
    encode_signed,
    encode_unknown,
    read_vint,
)
from mkvkit.isobmff.boxes import stco
from mkvkit.timing.rewrite import TimestampError, build_timing


@pytest.mark.parametrize("length", range(1, 9))
def test_vint_size_and_unknown_boundaries(length):
    limit = (1 << (7 * length)) - 2
    for value in (0, 1, limit):
        encoded = encode_size(value, length)
        assert read_vint(encoded) == (value, length, False)
    encoded = encode_unknown(length)
    value, decoded_length, unknown = read_vint(encoded)
    assert decoded_length == length
    assert unknown is True
    assert value == (1 << (7 * length)) - 1


def test_signed_lacing_round_trip():
    for value in (-8191, -1, 0, 1, 8191):
        encoded = encode_signed(value)
        raw, length, _ = read_vint(encoded)
        assert decode_signed(raw, length) == value


def test_vint_rejects_invalid_and_truncated_input():
    with pytest.raises(VintInvalid):
        read_vint(b"\x00")
    with pytest.raises(VintTruncated):
        read_vint(b"\x40")


def test_element_ids_preserve_marker():
    assert encode_id(0xEC) == b"\xec"
    assert read_vint(b"\xec", strip_marker=False)[0] == 0xEC


def test_stco_and_co64_layouts():
    narrow = stco([0x01020304], co64=False)
    wide = stco([0x0102030405060708], co64=True)
    assert narrow[4:8] == b"stco" and narrow[-4:] == b"\x01\x02\x03\x04"
    assert wide[4:8] == b"co64"
    assert wide[-8:] == b"\x01\x02\x03\x04\x05\x06\x07\x08"


def test_repair_budget_is_checked_after_dts_rebuild():
    repeated = [0, 33, 66, 99] * 30
    with pytest.raises(TimestampError):
        build_timing([value * 1_000_000 for value in repeated], 90000)
