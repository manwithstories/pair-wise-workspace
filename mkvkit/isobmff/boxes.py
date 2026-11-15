"""ISO Base Media File Format box serialization.

All builders return immutable ``bytes``; container sizes are derived from
payload length so a moov built with placeholder chunk offsets has the
same size as the final patched one.  ``stco`` vs ``co64`` is selected by
the caller up front (offsets only change values, not entry counts).
"""
from __future__ import annotations

import struct

UNITY_MATRIX = struct.pack(
    ">9I", 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000
)


def box(typ: bytes, payload: bytes) -> bytes:
    return struct.pack(">I4s", 8 + len(payload), typ) + payload


def full_box(typ: bytes, version: int, flags: int, payload: bytes) -> bytes:
    return box(typ, struct.pack(">B", version) + flags.to_bytes(3, "big") + payload)


def ftyp(brands=(b"isom", b"iso2", b"avc1", b"mp41"), minor=0x200) -> bytes:
    return box(b"ftyp", brands[0] + struct.pack(">I", minor) + b"".join(brands))


def mvhd(timescale: int, duration: int, next_track_id: int) -> bytes:
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">I", 0x00010000)  # rate 1.0
    payload += struct.pack(">H", 0x0100) + b"\x00\x00"  # volume 1.0
    payload += b"\x00" * 8 + UNITY_MATRIX + b"\x00" * 24
    payload += struct.pack(">I", next_track_id)
    return full_box(b"mvhd", 0, 0, payload)


def tkhd(track_id: int, duration: int, width: int, height: int,
         volume: int) -> bytes:
    payload = struct.pack(">II", 0, 0)
    payload += struct.pack(">I", track_id) + b"\x00" * 4
    payload += struct.pack(">I", duration)
    payload += b"\x00" * 8
    payload += struct.pack(">hhhh", 0, 0, volume, 0)
    payload += UNITY_MATRIX
    payload += struct.pack(">II", width << 16, height << 16)
    return full_box(b"tkhd", 0, 0x03, payload)


def mdhd(timescale: int, duration: int, language: str = "und") -> bytes:
    aliases = {"zh": "zho", "chi": "zho", "en": "eng", "ja": "jpn"}
    code = language.lower().split("-", 1)[0]
    code = aliases.get(code, code)
    if len(code) != 3 or not code.isascii() or not code.isalpha():
        code = "und"
    lang = 0
    for ch in code:
        lang = (lang << 5) | (ord(ch) - 0x60)
    payload = struct.pack(">IIIIHH", 0, 0, timescale, duration, lang, 0)
    return full_box(b"mdhd", 0, 0, payload)


def hdlr(handler: bytes, name: str) -> bytes:
    payload = b"\x00" * 4 + handler + b"\x00" * 12 + name.encode() + b"\x00"
    return full_box(b"hdlr", 0, 0, payload)


def vmhd() -> bytes:
    return full_box(b"vmhd", 0, 1, struct.pack(">HHHH", 0, 0, 0, 0))


def smhd() -> bytes:
    return full_box(b"smhd", 0, 0, struct.pack(">hH", 0, 0))


def nmhd() -> bytes:
    return full_box(b"nmhd", 0, 0, b"")


def dinf() -> bytes:
    url = full_box(b"url ", 0, 1, b"")
    return box(b"dinf", full_box(b"dref", 0, 0, struct.pack(">I", 1) + url))


def _descriptor(tag: int, body: bytes) -> bytes:
    n = len(body)
    if n < 0x80:
        length = bytes([n])
    else:
        length = bytes([0x80 | ((n >> 8) & 0x7F), n & 0xFF])
    return bytes([tag]) + length + body


def esds(codec_private: bytes) -> bytes:
    specific = _descriptor(0x05, codec_private)
    config = _descriptor(
        0x04,
        struct.pack(">B", 0x40) + b"\x15" + b"\x00\x06\x00"
        + struct.pack(">II", 0, 0) + specific,
    )
    sl = _descriptor(0x06, b"\x02")
    es = _descriptor(0x03, struct.pack(">HB", 1, 0) + config + sl)
    return full_box(b"esds", 0, 0, es)


def stsd_avc1(width: int, height: int, codec_private: bytes) -> bytes:
    payload = b"\x00" * 6 + struct.pack(">H", 1) + b"\x00" * 16
    payload += struct.pack(">HH", width, height)
    payload += struct.pack(">II", 0x00480000, 0x00480000)
    payload += b"\x00" * 4 + struct.pack(">H", 1) + b"\x00" * 32
    payload += struct.pack(">Hh", 0x18, -1)
    payload += box(b"avcC", codec_private)
    return full_box(b"stsd", 0, 0, struct.pack(">I", 1) + box(b"avc1", payload))


def stsd_mp4a(channels: int, sample_rate: int, codec_private: bytes) -> bytes:
    payload = b"\x00" * 6 + struct.pack(">H", 1) + b"\x00" * 8
    payload += struct.pack(">HHHHI", channels, 16, 0, 0, sample_rate << 16)
    payload += esds(codec_private)
    return full_box(b"stsd", 0, 0, struct.pack(">I", 1) + box(b"mp4a", payload))


def stsd_tx3g() -> bytes:
    payload = b"\x00" * 6 + struct.pack(">H", 1)
    payload += struct.pack(">I", 0) + b"\x00\x00" + b"\x00" * 4
    payload += struct.pack(">hhhh", 0, 0, 0, 0)
    payload += struct.pack(">HHHBB", 0, 0, 1, 0, 12) + b"\x00" * 4
    ftab = box(b"ftab", struct.pack(">HHB", 1, 1, 9) + b"monospace")
    return full_box(b"stsd", 0, 0, struct.pack(">I", 1) + box(b"tx3g", payload + ftab))


def stts(entries) -> bytes:
    payload = struct.pack(">I", len(entries))
    payload += b"".join(struct.pack(">II", c, d) for c, d in entries)
    return full_box(b"stts", 0, 0, payload)


def ctts(entries) -> bytes:
    payload = struct.pack(">I", len(entries))
    payload += b"".join(struct.pack(">II", c, v) for c, v in entries)
    return full_box(b"ctts", 0, 0, payload)


def stss(indices) -> bytes:
    payload = struct.pack(">I", len(indices))
    payload += b"".join(struct.pack(">I", i) for i in indices)
    return full_box(b"stss", 0, 0, payload)


def stsc(entries) -> bytes:
    payload = struct.pack(">I", len(entries))
    payload += b"".join(struct.pack(">III", *e) for e in entries)
    return full_box(b"stsc", 0, 0, payload)


def stsz(sizes) -> bytes:
    payload = struct.pack(">II", 0, len(sizes))
    payload += b"".join(struct.pack(">I", s) for s in sizes)
    return full_box(b"stsz", 0, 0, payload)


def stco(offsets, co64: bool = False) -> bytes:
    if co64:
        payload = struct.pack(">I", len(offsets))
        payload += b"".join(struct.pack(">Q", o) for o in offsets)
        return full_box(b"co64", 0, 0, payload)
    payload = struct.pack(">I", len(offsets))
    payload += b"".join(struct.pack(">I", o) for o in offsets)
    return full_box(b"stco", 0, 0, payload)


def elst(media_time: int, segment_duration: int, empty_duration: int = 0) -> bytes:
    count = 2 if empty_duration else 1
    payload = struct.pack(">I", count)
    if empty_duration:
        payload += struct.pack(">IiHh", empty_duration, -1, 1, 0)
    payload += struct.pack(">IiHh", segment_duration, media_time, 1, 0)
    return full_box(b"elst", 0, 0, payload)


def edts(media_time: int, segment_duration: int, empty_duration: int = 0) -> bytes:
    return box(b"edts", elst(media_time, segment_duration, empty_duration))


def mdat_header(payload_size: int) -> bytes:
    if payload_size + 8 < 0x100000000:
        return struct.pack(">I4s", payload_size + 8, b"mdat")
    return struct.pack(">I4sQ", 1, b"mdat", payload_size + 16)
