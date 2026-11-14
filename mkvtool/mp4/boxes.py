"""Minimal ISO BMFF box encoding and parsing helpers."""

from __future__ import annotations

from dataclasses import dataclass

from mkvtool.errors import InputParseError


def u8(value: int) -> bytes:
    return int(value).to_bytes(1, "big")


def u16(value: int) -> bytes:
    return int(value).to_bytes(2, "big")


def u24(value: int) -> bytes:
    return int(value).to_bytes(3, "big")


def u32(value: int) -> bytes:
    return int(value).to_bytes(4, "big")


def u64(value: int) -> bytes:
    return int(value).to_bytes(8, "big")


def i32(value: int) -> bytes:
    return int(value).to_bytes(4, "big", signed=True)


def box(box_type: str | bytes, payload: bytes = b"") -> bytes:
    kind = box_type.encode("ascii") if isinstance(box_type, str) else box_type
    if len(kind) != 4:
        raise ValueError("box type must contain four ASCII bytes")
    total = 8 + len(payload)
    if total > 0xFFFFFFFF:
        raise ValueError("box exceeds 32-bit size")
    return u32(total) + kind + payload


def full_box(box_type: str | bytes, version: int = 0, flags: int = 0, payload: bytes = b"") -> bytes:
    if not 0 <= version <= 255 or not 0 <= flags <= 0xFFFFFF:
        raise ValueError("invalid full-box version/flags")
    return box(box_type, u8(version) + u24(flags) + payload)


IDENTITY_MATRIX = (
    u32(0x00010000) + u32(0) + u32(0)
    + u32(0) + u32(0x00010000) + u32(0)
    + u32(0) + u32(0) + u32(0x40000000)
)


@dataclass
class Mp4Box:
    type: bytes
    offset: int
    size: int
    header_size: int
    data: bytes

    @property
    def payload(self) -> bytes:
        return self.data[self.offset + self.header_size : self.offset + self.size]

    @property
    def version(self) -> int:
        payload = self.payload
        return payload[0] if payload else 0

    @property
    def flags(self) -> int:
        payload = self.payload
        return int.from_bytes(payload[1:4], "big") if self.header_size == 16 else 0

    def children(self) -> list["Mp4Box"]:
        start = self.offset + self.header_size
        return parse_boxes(self.data, start, self.offset + self.size)


CONTAINERS = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"dinf", b"edts", b"udta"}


def parse_boxes(data: bytes, start: int = 0, end: int | None = None) -> list[Mp4Box]:
    end = len(data) if end is None else end
    boxes: list[Mp4Box] = []
    cursor = start
    while cursor < end:
        if cursor + 8 > end:
            raise InputParseError(f"truncated MP4 box header at offset {cursor}")
        size = int.from_bytes(data[cursor : cursor + 4], "big")
        kind = data[cursor + 4 : cursor + 8]
        header = 8
        if size == 1:
            if cursor + 16 > end:
                raise InputParseError(f"truncated 64-bit MP4 box size at offset {cursor}")
            size = int.from_bytes(data[cursor + 8 : cursor + 16], "big")
            header = 16
        elif size == 0:
            size = end - cursor
        if size < header or cursor + size > end:
            raise InputParseError(f"invalid MP4 box {kind!r} size at offset {cursor}")
        boxes.append(Mp4Box(kind, cursor, size, header, data))
        cursor += size
    return boxes


def find(boxes: list[Mp4Box], box_type: bytes) -> Mp4Box | None:
    return next((item for item in boxes if item.type == box_type), None)


def require(boxes: list[Mp4Box], box_type: bytes) -> Mp4Box:
    found = find(boxes, box_type)
    if found is None:
        raise InputParseError(f"required MP4 box {box_type!r} is missing")
    return found


def ftyp(major: str = "isom", compatible: tuple[str, ...] = ("isom", "iso2", "avc1", "mp41")) -> bytes:
    brands = b"".join(item.encode("ascii") for item in compatible)
    return box("ftyp", major.encode("ascii") + u32(512) + brands)


def mdat(payload: bytes) -> bytes:
    return box("mdat", payload)


def language_code(code: str) -> int:
    value = (code or "und").lower()[:3]
    value = value.ljust(3, "d")
    packed = 0
    for character in value:
        packed = (packed << 5) | (ord(character) - 0x60)
    return packed


def avc1_sample_entry(width: int, height: int, avcc: bytes) -> bytes:
    if avcc.startswith(b"avcC") and len(avcc) >= 8:
        declared = int.from_bytes(avcc[:4], "big")
        avcc = avcc[8 : declared] if declared >= 8 else avcc[8:]
    config = box("avcC", avcc)
    compressor = b"\x0b" + b"mkvtool avc".ljust(31, b"\x00")
    entry = (
        b"\x00" * 6 + u16(1) + b"\x00" * 16
        + u16(width) + u16(height) + u32(0x00480000) + u32(0x00480000)
        + u32(0) + u16(1) + compressor + u16(0x0018) + b"\xff\xff"
    )
    return box("avc1", entry + config)


def _descriptor(tag: int, payload: bytes) -> bytes:
    size = len(payload)
    if size < 0x80:
        length = bytes([size])
    elif size < 0x4000:
        length = bytes([0x80 | (size >> 7), size & 0x7F])
    else:
        length = bytes([0x80 | (size >> 14), 0x80 | ((size >> 7) & 0x7F), size & 0x7F])
    return bytes([tag]) + length + payload


def esds(asc: bytes) -> bytes:
    decoder_specific = _descriptor(0x05, asc)
    decoder_config = _descriptor(
        0x04,
        b"\x40\x15" + u24(0) + u32(0) + u32(0) + decoder_specific,
    )
    sl_config = _descriptor(0x06, b"\x02")
    es_descriptor = _descriptor(0x03, u16(1) + b"\x00" + decoder_config + sl_config)
    return full_box("esds", 0, 0, es_descriptor)


def mp4a_sample_entry(channels: int, sample_rate: int, asc: bytes) -> bytes:
    rate = min(sample_rate, 65535) << 16
    entry = (
        b"\x00" * 6 + u16(1) + b"\x00" * 8
        + u16(max(1, channels)) + u16(16) + u16(0) + u16(0)
        + u32(rate) + esds(asc)
    )
    return box("mp4a", entry)


def build_stsd(sample_entry: bytes) -> bytes:
    return full_box("stsd", 0, 0, u32(1) + sample_entry)


def build_stts(durations: list[int]) -> bytes:
    entries: list[tuple[int, int]] = []
    for duration in durations:
        if entries and entries[-1][1] == duration:
            entries[-1] = (entries[-1][0] + 1, duration)
        else:
            entries.append((1, duration))
    payload = u32(len(entries)) + b"".join(u32(count) + u32(delta) for count, delta in entries)
    return full_box("stts", 0, 0, payload)


def build_ctts(offsets: list[int]) -> bytes:
    entries: list[tuple[int, int]] = []
    for offset in offsets:
        if entries and entries[-1][1] == offset:
            entries[-1] = (entries[-1][0] + 1, offset)
        else:
            entries.append((1, offset))
    payload = u32(len(entries)) + b"".join(u32(count) + u32(offset) for count, offset in entries)
    return full_box("ctts", 1, 0, payload)


def build_stss(sync_samples: list[int]) -> bytes:
    payload = u32(len(sync_samples)) + b"".join(u32(index) for index in sync_samples)
    return full_box("stss", 0, 0, payload)


def build_stsc(chunks: int, samples_per_chunk: int) -> bytes:
    return full_box("stsc", 0, 0, u32(1) + u32(1) + u32(samples_per_chunk) + u32(1))


def build_stco(offsets: list[int]) -> bytes:
    return full_box("stco", 0, 0, u32(len(offsets)) + b"".join(u32(offset) for offset in offsets))


def build_stsz(sizes: list[int]) -> bytes:
    return full_box("stsz", 0, 0, u32(0) + u32(len(sizes)) + b"".join(u32(size) for size in sizes))
