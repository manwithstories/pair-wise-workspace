"""Tiny deterministic Matroska writer used only by fixture generation."""

from __future__ import annotations

import struct
import zlib

from mkvmp4.ebml.ids import (
    CHAPTERS, CLUSTER, CRC32, CUES, EBML, INFO, SEGMENT, TAGS, TRACKS, VOID,
)
from mkvmp4.ebml.vint import encode_element_id, encode_signed, encode_vint


def element(element_id: int, payload: bytes = b"", *, unknown: bool = False) -> bytes:
    header = encode_element_id(element_id)
    return header + (b"\xff" if unknown else encode_vint(len(payload))) + payload


def uint_element(element_id: int, value: int) -> bytes:
    width = max(1, (value.bit_length() + 7) // 8)
    return element(element_id, value.to_bytes(width, "big"))


def int_element(element_id: int, value: int) -> bytes:
    return element(element_id, encode_signed(value))


def float_element(element_id: int, value: float) -> bytes:
    return element(element_id, struct.pack(">d", value))


def string_element(element_id: int, value: str) -> bytes:
    return element(element_id, value.encode("utf-8"))


def ebml_header() -> bytes:
    payload = b"".join([
        uint_element(0x4286, 1), uint_element(0x42F7, 1),
        uint_element(0x42F2, 4), uint_element(0x42F3, 8),
        string_element(0x4282, "matroska"), uint_element(0x4287, 4),
        uint_element(0x4285, 2),
    ])
    return element(EBML, payload)


def track_entry(number: int, track_type: int, codec: str, private: bytes, *,
                name: str = "", language: str = "und", default: bool = True,
                forced: bool = False, duration_ns: int | None = None,
                width: int = 0, height: int = 0, sample_rate: float = 0,
                channels: int = 0) -> bytes:
    fields = [
        uint_element(0xD7, number), uint_element(0x73C5, 1000 + number),
        uint_element(0x83, track_type), string_element(0x86, codec),
        element(0x63A2, private), string_element(0x22B59C, language),
        uint_element(0x88, int(default)), uint_element(0x55AA, int(forced)),
    ]
    if name:
        fields.append(string_element(0x536E, name))
    if duration_ns:
        fields.append(uint_element(0x23E383, duration_ns))
    if width or height:
        fields.append(element(0xE0, uint_element(0xB0, width) + uint_element(0xBA, height)))
    if sample_rate or channels:
        fields.append(element(0xE1, float_element(0xB5, sample_rate)
                              + uint_element(0x9F, channels)))
    return element(0xAE, b"".join(fields))


def info(timestamp_scale: int, duration_ticks: float, title: str) -> bytes:
    return element(INFO, b"".join([
        uint_element(0x2AD7B1, timestamp_scale), float_element(0x4489, duration_ticks),
        string_element(0x4D80, "mkvmp4-fixtures"), string_element(0x5741, "mkvmp4"),
        string_element(0x7BA9, title),
    ]))


def tracks(track_blobs: list[bytes]) -> bytes:
    return element(TRACKS, b"".join(track_blobs))


def _lace_xiph(frames: list[bytes]) -> bytes:
    count = len(frames) - 1
    sizes = bytearray([count])
    for frame in frames[:-1]:
        remaining = len(frame)
        while remaining >= 255:
            sizes.append(255)
            remaining -= 255
        sizes.append(remaining)
    return bytes(sizes) + b"".join(frames)


def _lace_fixed(frames: list[bytes]) -> bytes:
    body = b"".join(frames)
    return bytes([len(frames) - 1]) + body


def _lace_ebml(frames: list[bytes]) -> bytes:
    first = encode_vint(len(frames[0]))
    sizes = bytearray([len(frames) - 1]) + bytearray(first)
    for previous, current in zip(frames, frames[1:-1]):
        sizes += encode_signed(len(current) - len(previous), len(first))
    return bytes(sizes) + b"".join(frames)


def simple_block(track_number: int, relative_ticks: int, frames: list[bytes],
                 lacing: int = 0, keyframe: bool = True) -> bytes:
    flags = (0x80 if keyframe else 0) | (lacing << 1)
    prefix = encode_vint(track_number) + int(relative_ticks).to_bytes(2, "big", signed=True)
    if lacing == 1:
        body = _lace_xiph(frames)
    elif lacing == 2:
        body = _lace_fixed(frames)
    elif lacing == 3:
        body = _lace_ebml(frames)
    else:
        body = frames[0]
    return element(0xA3, prefix + bytes([flags]) + body)


def block_group(track_number: int, relative_ticks: int, frame: bytes,
                duration_ticks: int, references: list[int], keyframe: bool = False) -> bytes:
    prefix = encode_vint(track_number) + int(relative_ticks).to_bytes(2, "big", signed=True)
    flags = 0x80 if keyframe else 0
    fields = [element(0xA1, prefix + bytes([flags]) + frame),
              uint_element(0x9B, duration_ticks)]
    fields.extend(int_element(0xFB, reference) for reference in references)
    return element(0xA0, b"".join(fields))


def cluster(timestamp_ticks: int, blocks: list[bytes], *, unknown: bool = True,
            with_crc: bool = False) -> bytes:
    body = uint_element(0xE7, timestamp_ticks) + b"".join(blocks)
    if with_crc:
        crc = element(CRC32, zlib.crc32(body).to_bytes(4, "big"))
        body = crc + body
    return element(CLUSTER, body, unknown=unknown)


def chapter_atom(uid: int, start_ticks: int, end_ticks: int, title: str,
                 language: str = "zho") -> bytes:
    display = element(0x80, string_element(0x85, title)
                      + string_element(0x437C, language))
    return element(0xB6, b"".join([
        uint_element(0x73C4, uid), uint_element(0x91, start_ticks),
        uint_element(0x92, end_ticks), display,
    ]))


def chapters(atoms: list[bytes], *, unknown: bool = True) -> bytes:
    edition = element(0x45B9, b"".join(atoms))
    return element(CHAPTERS, edition, unknown=unknown)


def tags(values: list[tuple[str, str]]) -> bytes:
    simple = [element(0x67C8, string_element(0x45A3, name) + string_element(0x4487, value)
                      + string_element(0x447A, "und") + uint_element(0x4484, 1))
              for name, value in values]
    return element(TAGS, element(0x7373, element(0x63C0, b"") + b"".join(simple)))


def cues(count: int) -> bytes:
    points = []
    for index in range(count):
        positions = element(0xB7, uint_element(0xF7, 1) + uint_element(0xF1, 0))
        points.append(element(0xBB, uint_element(0xB3, index * 2000) + positions))
    return element(CUES, b"".join(points))


def void(payload: bytes = b"") -> bytes:
    return element(VOID, payload)

