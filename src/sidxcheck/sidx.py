"""Segment index (sidx) parsing and encoding."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from pathlib import Path

from .boxes import Box


class SidxError(ValueError):
    """Raised when a sidx payload cannot be decoded."""


@dataclass(slots=True)
class SidxEntry:
    index: int
    reference_type: int
    referenced_size: int
    subsegment_duration: int
    starts_with_sap: int = 1
    sap_type: int = 1
    sap_delta_time: int = 0

    @property
    def is_hierarchical(self) -> bool:
        return self.reference_type == 1


@dataclass(slots=True)
class SidxBox:
    file: Path | None
    box_offset: int
    box_size: int
    version: int
    reference_id: int
    timescale: int
    earliest_presentation_time: int
    first_offset: int
    declared_reference_count: int
    entries: list[SidxEntry] = field(default_factory=list)
    issues: list[str] = field(default_factory=list)

    @property
    def anchor_offset(self) -> int:
        return self.box_offset + self.box_size

    @property
    def sample_entries(self) -> list[SidxEntry]:
        return [entry for entry in self.entries if not entry.is_hierarchical]

    @property
    def total_duration(self) -> int:
        return sum(entry.subsegment_duration for entry in self.sample_entries)


def parse_sidx(box: Box, data: memoryview | bytes, *, file: Path | None = None) -> SidxBox:
    if box.type != "sidx":
        raise SidxError(f"expected sidx box, got {box.type!r}")
    if box.end > len(data):
        raise SidxError("sidx box extends beyond available data")
    payload = memoryview(data)[box.payload_start:box.end]
    if len(payload) < 12:
        raise SidxError("sidx payload is shorter than 12 bytes")
    version = payload[0]
    if version not in (0, 1):
        raise SidxError(f"unsupported sidx version {version}")
    cursor = 4
    reference_id, timescale = struct.unpack_from(">II", payload, cursor)
    cursor += 8
    if version == 0:
        if len(payload) < cursor + 12:
            raise SidxError("truncated version 0 sidx header")
        earliest_presentation_time, first_offset = struct.unpack_from(">II", payload, cursor)
        cursor += 8
    else:
        if len(payload) < cursor + 20:
            raise SidxError("truncated version 1 sidx header")
        earliest_presentation_time, first_offset = struct.unpack_from(">QQ", payload, cursor)
        cursor += 16
    cursor += 2
    if len(payload) < cursor + 2:
        raise SidxError("truncated sidx reference count")
    declared_count = struct.unpack_from(">H", payload, cursor)[0]
    cursor += 2
    available = (len(payload) - cursor) // 12
    parse_count = min(declared_count, available)
    issues: list[str] = []
    if declared_count > available:
        issues.append(f"reference_count {declared_count} exceeds available {available}")
    elif available > declared_count:
        issues.append(f"reference_count {declared_count} leaves {available - declared_count} entries unread")

    entries: list[SidxEntry] = []
    for index in range(parse_count):
        first, duration, flags = struct.unpack_from(">III", payload, cursor)
        cursor += 12
        entries.append(
            SidxEntry(
                index=index,
                reference_type=(first >> 31) & 1,
                referenced_size=first & 0x7FFFFFFF,
                subsegment_duration=duration,
                starts_with_sap=(flags >> 31) & 1,
                sap_type=(flags >> 28) & 0x7,
                sap_delta_time=flags & 0x0FFFFFFF,
            )
        )
    return SidxBox(
        file=file,
        box_offset=box.start,
        box_size=box.size,
        version=version,
        reference_id=reference_id,
        timescale=timescale,
        earliest_presentation_time=earliest_presentation_time,
        first_offset=first_offset,
        declared_reference_count=declared_count,
        entries=entries,
        issues=issues,
    )


def encode_sidx(
    *,
    reference_id: int,
    timescale: int,
    earliest_presentation_time: int,
    first_offset: int,
    entries: list[SidxEntry],
    version: int | None = None,
    declared_reference_count: int | None = None,
) -> bytes:
    if not 0 <= reference_id <= 0xFFFFFFFF:
        raise SidxError("reference_id must fit in 32 bits")
    if not 0 <= timescale <= 0xFFFFFFFF:
        raise SidxError("timescale must fit in 32 bits")
    if earliest_presentation_time < 0 or first_offset < 0:
        raise SidxError("sidx times and offsets must be non-negative")
    if earliest_presentation_time > 0xFFFFFFFFFFFFFFFF or first_offset > 0xFFFFFFFFFFFFFFFF:
        raise SidxError("sidx version 1 fields must fit in 64 bits")
    if version is None:
        version = 1 if earliest_presentation_time > 0xFFFFFFFF or first_offset > 0xFFFFFFFF else 0
    if version not in (0, 1):
        raise SidxError(f"unsupported sidx version {version}")
    if version == 0 and (earliest_presentation_time > 0xFFFFFFFF or first_offset > 0xFFFFFFFF):
        raise SidxError("version 0 sidx cannot encode 64-bit fields")
    count = len(entries) if declared_reference_count is None else declared_reference_count
    if not 0 <= count <= 0xFFFF:
        raise SidxError("reference count must fit in 16 bits")

    body = bytearray()
    body += struct.pack(">B3xII", version, reference_id, timescale)
    if version == 0:
        body += struct.pack(">II", earliest_presentation_time, first_offset)
    else:
        body += struct.pack(">QQ", earliest_presentation_time, first_offset)
    body += struct.pack(">HH", 0, count)
    for entry in entries:
        if not 0 <= entry.referenced_size <= 0x7FFFFFFF:
            raise SidxError(f"referenced_size out of range at entry {entry.index}")
        if not 0 <= entry.subsegment_duration <= 0xFFFFFFFF:
            raise SidxError(f"subsegment_duration out of range at entry {entry.index}")
        first = ((entry.reference_type & 1) << 31) | entry.referenced_size
        flags = (
            ((entry.starts_with_sap & 1) << 31)
            | ((entry.sap_type & 0x7) << 28)
            | (entry.sap_delta_time & 0x0FFFFFFF)
        )
        body += struct.pack(">III", first, entry.subsegment_duration, flags)
    return struct.pack(">I4s", len(body) + 8, b"sidx") + bytes(body)
