"""Small deterministic EBML writer used only by fixture generation."""

from __future__ import annotations

import struct
import zlib


UNKNOWN_SIZE = b"\x01" + b"\xff" * 7


def id_bytes(eid: int) -> bytes:
    width = max(1, (eid.bit_length() + 7) // 8)
    return eid.to_bytes(width, "big")


def vint(value: int, width: int | None = None) -> bytes:
    if value < 0:
        raise ValueError("VINT cannot encode a negative value")
    if width is None:
        for candidate in range(1, 9):
            if value < (1 << (7 * candidate)) - 1:
                width = candidate
                break
        else:
            raise ValueError("VINT exceeds 8 bytes")
    assert width is not None
    encoded = value | (1 << (7 * width))
    return encoded.to_bytes(width, "big")


def element(eid: int, payload: bytes, *, unknown: bool = False) -> bytes:
    size = UNKNOWN_SIZE if unknown else vint(len(payload))
    return id_bytes(eid) + size + payload


def uint(value: int, width: int | None = None) -> bytes:
    if width is None:
        width = max(1, (value.bit_length() + 7) // 8)
    return value.to_bytes(width, "big", signed=False)


def sint(value: int, width: int = 2) -> bytes:
    return value.to_bytes(width, "big", signed=True)


def f64(value: float) -> bytes:
    return struct.pack(">d", value)


def text(value: str) -> bytes:
    return value.encode("utf-8")


def master(eid: int, children: list[bytes], *, unknown: bool = False) -> bytes:
    return element(eid, b"".join(children), unknown=unknown)


def crc32_master(
    eid: int,
    children: list[bytes],
    *,
    unknown: bool = False,
    corrupt: bool = False,
) -> bytes:
    body = b"".join(children)
    value = zlib.crc32(body) & 0xFFFFFFFF
    if corrupt:
        value ^= 0xFFFFFFFF
    crc = element(0xBF, value.to_bytes(4, "big"))
    return element(eid, crc + body, unknown=unknown)


def simple_block(track: int, relative_ms: int, data: bytes, *, keyframe: bool) -> bytes:
    flags = 0x80 if keyframe else 0x00
    payload = vint(track, 1) + sint(relative_ms, 2) + bytes([flags]) + data
    return element(0xA3, payload)


def block_group(
    track: int,
    relative_ms: int,
    data: bytes,
    *,
    duration_ms: int,
    reference: int | None = None,
) -> bytes:
    block = element(0xA1, vint(track, 1) + sint(relative_ms, 2) + b"\x00" + data)
    children = [block, element(0x9B, uint(duration_ms))]
    if reference is not None:
        children.append(element(0xFB, sint(reference, 2)))
    return master(0xA0, children)


def video_track(
    number: int,
    uid: int,
    width: int,
    height: int,
    *,
    default_duration_ns: int,
    codec_private: bytes,
    name: str = "video",
) -> bytes:
    return master(
        0xAE,
        [
            element(0xD7, uint(number)),
            element(0x73C5, uint(uid)),
            element(0x83, uint(1)),
            element(0x86, text("V_MPEG4/ISO/AVC")),
            element(0x63A2, codec_private),
            element(0x23E383, uint(default_duration_ns)),
            element(0x536E, text(name)),
            element(0x22B59C, text("und")),
            master(0xE0, [element(0xB0, uint(width)), element(0xBA, uint(height))]),
        ],
    )


def audio_track(number: int, uid: int, *, default_duration_ns: int, codec_private: bytes) -> bytes:
    return master(
        0xAE,
        [
            element(0xD7, uint(number)),
            element(0x73C5, uint(uid)),
            element(0x83, uint(2)),
            element(0x86, text("A_AAC")),
            element(0x63A2, codec_private),
            element(0x23E383, uint(default_duration_ns)),
            element(0x536E, text("audio")),
            element(0x22B59C, text("und")),
            master(
                0xE1,
                [element(0xB5, f64(48000.0)), element(0x9F, uint(2)), element(0x6264, uint(16))],
            ),
        ],
    )


def subtitle_track(number: int, uid: int) -> bytes:
    return master(
        0xAE,
        [
            element(0xD7, uint(number)),
            element(0x73C5, uint(uid)),
            element(0x83, uint(17)),
            element(0x86, text("S_TEXT/UTF8")),
            element(0x536E, text("captions")),
        ],
    )


def info(duration_ms: float) -> bytes:
    return master(
        0x1549A966,
        [
            element(0x2AD7B1, uint(1_000_000)),
            element(0x4489, f64(duration_ms)),
            element(0x4D80, text("mkvtomp4-fixtures")),
            element(0x5741, text("mkvtomp4-fixtures")),
        ],
    )


def ebml_header() -> bytes:
    return master(
        0x1A45DFA3,
        [
            element(0x4286, uint(1)),
            element(0x42F7, uint(1)),
            element(0x42F2, uint(4)),
            element(0x42F3, uint(8)),
            element(0x4282, text("matroska")),
            element(0x4287, uint(4)),
            element(0x4285, uint(2)),
        ],
    )


def tags_for_depth() -> bytes:
    tag_name = element(0x45A3, text("archive"))
    inner = master(0x67C8, [tag_name])
    outer = master(0x67C8, [inner])
    tag = master(0x7373, [outer])
    return master(0x1254C367, [tag])
