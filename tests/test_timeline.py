from __future__ import annotations

import pytest

from mkvmp4.ebml.vint import encode_signed, encode_vint
from mkvmp4.mkv.timeline import expand_laces, ns_to_units


def block(track: int, frames: list[bytes], lacing: int = 0) -> bytes:
    flags = lacing << 1
    prefix = encode_vint(track) + b"\x00\x00" + bytes([flags])
    if lacing == 0:
        body = frames[0]
    elif lacing == 1:
        sizes = bytearray([len(frames) - 1])
        for frame in frames[:-1]:
            remaining = len(frame)
            while remaining >= 255:
                sizes.append(255)
                remaining -= 255
            sizes.append(remaining)
        body = bytes(sizes) + b"".join(frames)
    elif lacing == 2:
        body = bytes([len(frames) - 1]) + b"".join(frames)
    else:
        first = encode_vint(len(frames[0]))
        body = bytes([len(frames) - 1]) + first
        body += b"".join(encode_signed(len(cur) - len(prev), len(first))
                         for prev, cur in zip(frames, frames[1:-1]))
        body += b"".join(frames)
    return prefix + body


def test_no_lace() -> None:
    _, frames = expand_laces(block(1, [b"hello"]))
    assert [bytes(frame) for frame in frames] == [b"hello"]


@pytest.mark.parametrize(("lacing", "original"), [
    (1, [b"a" * 260, b"b" * 3, b"c" * 7]),
    (2, [b"a" * 10, b"b" * 10, b"c" * 10]),
    (3, [b"a" * 260, b"b" * 3, b"c" * 7]),
])
def test_all_lace_modes(lacing: int, original: list[bytes]) -> None:
    header, frames = expand_laces(block(1, original, lacing))
    assert header.track_number == 1
    assert [bytes(frame) for frame in frames] == original


def test_cumulative_rounding_has_no_per_frame_drift() -> None:
    duration_ns = 1_000_001
    timescale = 1_000_000
    final = ns_to_units(1000 * duration_ns, timescale)
    per_frame = 1000 * ns_to_units(duration_ns, timescale)
    assert abs(final - per_frame) <= 1
    assert abs(final - (1000 * duration_ns * timescale / 1_000_000_000)) <= 0.5

