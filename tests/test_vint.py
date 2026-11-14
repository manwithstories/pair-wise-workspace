from __future__ import annotations

import io

import pytest

from mkvmp4.ebml.vint import (
    VINTError, decode_signed, decode_vint, encode_element_id, encode_signed,
    encode_unknown_size, encode_vint, read_element_id, read_vint,
)


@pytest.mark.parametrize(
    ("value", "width", "raw"),
    [(0, 1, "80"), (126, 1, "fe"), (127, 2, "407f"), (16382, 2, "7ffe"),
     (16383, 3, "203fff")],
)
def test_vint_round_trip(value: int, width: int, raw: str) -> None:
    encoded = encode_vint(value, width)
    assert encoded.hex() == raw
    assert decode_vint(encoded) == (value, width, False)


def test_unknown_size_sentinel_and_stream_reader() -> None:
    raw = encode_unknown_size(2)
    assert raw == b"\x7f\xff"
    assert decode_vint(raw) == (16383, 2, True)
    assert read_vint(io.BytesIO(raw)) == (16383, 2, True)


def test_element_id_keeps_marker_bits() -> None:
    stream = io.BytesIO(bytes.fromhex("1a45dfa3"))
    assert read_element_id(stream) == (0x1A45DFA3, 4)
    assert encode_element_id(0x1A45DFA3) == bytes.fromhex("1a45dfa3")


@pytest.mark.parametrize("value", [-128, -1, 0, 1, 127, 128, -32768, 32767])
def test_signed_round_trip(value: int) -> None:
    encoded = encode_signed(value)
    assert decode_signed(encoded) == value


def test_invalid_vint_inputs() -> None:
    with pytest.raises(VINTError):
        decode_vint(b"\x00")
    with pytest.raises(VINTError):
        decode_vint(b"\x40")
    with pytest.raises(VINTError):
        encode_vint(127, 1)
    with pytest.raises(VINTError):
        encode_signed(128, 1)

