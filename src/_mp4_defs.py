"""MP4 box definitions: errors, container tables, and the Box node + writer."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field


class Mp4Error(ValueError):
    """Raised when an MP4 structure is malformed or cannot be written."""

    def __init__(self, code: str, message: str, offset: int | None = None) -> None:
        self.code = code
        self.offset = offset
        super().__init__(message)

    def as_dict(self) -> dict[str, object]:
        return {"code": self.code, "message": str(self), "offset": self.offset}


#: Boxes whose payload is a sequence of child boxes.
CONTAINER_TYPES = frozenset(
    {
        b"moov",
        b"trak",
        b"mdia",
        b"minf",
        b"stbl",
        b"dinf",
        b"edts",
        b"udta",
        b"mvex",
        b"moof",
        b"traf",
    }
)

#: Boxes that are a FullBox but whose payload we write verbatim.
FULL_BOX_TYPES = frozenset(
    {
        b"mvhd",
        b"tkhd",
        b"mdhd",
        b"hdlr",
        b"vmhd",
        b"smhd",
        b"nmhd",
        b"stts",
        b"ctts",
        b"stsc",
        b"stsz",
        b"stco",
        b"co64",
        b"stss",
        b"elst",
        b"dref",
        b"url ",
        b"stsd",
        b"esds",
    }
)

#: Leaf boxes that may legally appear inside a sample entry's extra area.
SAMPLE_ENTRY_TYPES = frozenset({b"avc1", b"hvc1", b"mp4a", b"Opus", b"vp09", b"av01", b"tx3g"})

#: Fixed payload sizes of sample entries before their child config boxes.
SAMPLE_ENTRY_FIXED = {
    b"avc1": 78,
    b"hvc1": 78,
    b"vp09": 78,
    b"av01": 78,
    b"mp4a": 28,
    b"Opus": 28,
    b"tx3g": 0,
}


@dataclass
class Box:
    """One MP4 box, either a container of children or a leaf payload."""

    type: bytes
    payload: bytes = b""
    children: list["Box"] = field(default_factory=list)
    version: int | None = None
    flags: int = 0

    def __post_init__(self) -> None:
        if len(self.type) != 4:
            raise Mp4Error("box_type_length", f"box type must be 4 bytes, got {self.type!r}")

    @property
    def is_container(self) -> bool:
        return self.type in CONTAINER_TYPES

    def body(self) -> bytes:
        if self.type in CONTAINER_TYPES:
            return b"".join(serialize(child) for child in self.children)
        head = self.payload
        if self.version is not None:
            head = bytes([self.version]) + self.flags.to_bytes(3, "big") + head
        # Sample entries and entry-list boxes (stsd/dref) carry child boxes
        # after their fixed fields.
        if self.children:
            return head + b"".join(serialize(child) for child in self.children)
        return head

    def size(self) -> int:
        return 8 + len(self.body())

    def child(self, box_type: bytes) -> "Box | None":
        for item in self.children:
            if item.type == box_type:
                return item
        return None

    def walk(self):
        yield self
        for item in self.children:
            yield from item.walk()


def box(box_type: bytes, payload: bytes = b"", *, children: list[Box] | None = None) -> Box:
    return Box(type=box_type, payload=payload, children=list(children or []))


def full_box(
    box_type: bytes,
    version: int,
    flags: int,
    payload: bytes = b"",
    *,
    children: list[Box] | None = None,
) -> Box:
    if not 0 <= version <= 0xFF:
        raise Mp4Error("box_version", f"version {version} out of range")
    if not 0 <= flags <= 0xFFFFFF:
        raise Mp4Error("box_flags", f"flags {flags:#x} out of range")
    return Box(
        type=box_type,
        payload=payload,
        children=list(children or []),
        version=version,
        flags=flags,
    )


def serialize(root: Box) -> bytes:
    """Serialize a box tree to bytes."""
    return _serialize_box(root)


def _serialize_box(current: Box) -> bytes:
    body = current.body()
    total = 8 + len(body)
    if total > 0xFFFFFFFF:
        # 64-bit largesize form: size=1, type, largesize, payload.
        return b"\x00\x00\x00\x01" + current.type + struct.pack(">Q", 16 + len(body)) + body
    return struct.pack(">I", total) + current.type + body


__all__ = [
    "Box",
    "CONTAINER_TYPES",
    "FULL_BOX_TYPES",
    "Mp4Error",
    "SAMPLE_ENTRY_FIXED",
    "SAMPLE_ENTRY_TYPES",
    "box",
    "full_box",
    "serialize",
]
