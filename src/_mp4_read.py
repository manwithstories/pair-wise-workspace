"""MP4 box reading: records, parsing, and offset assignment."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field

from ._mp4_defs import (
    CONTAINER_TYPES,
    FULL_BOX_TYPES,
    SAMPLE_ENTRY_FIXED,
    SAMPLE_ENTRY_TYPES,
    Box,
    Mp4Error,
    serialize,
)


@dataclass
class BoxRecord:
    """One box as read back from a file, with absolute offsets."""

    type: bytes
    offset: int
    header_size: int
    size: int
    version: int | None = None
    flags: int = 0
    depth: int = 0
    children: list["BoxRecord"] = field(default_factory=list)

    @property
    def end(self) -> int:
        return self.offset + self.size

    @property
    def payload_offset(self) -> int:
        return self.offset + self.header_size

    def child(self, box_type: bytes) -> "BoxRecord | None":
        for item in self.children:
            if item.type == box_type:
                return item
        return None

    def find(self, box_type: bytes) -> "BoxRecord | None":
        for item in self.walk():
            if item.type == box_type:
                return item
        return None

    def walk(self):
        yield self
        for item in self.children:
            yield from item.walk()

    def as_dict(self) -> dict[str, object]:
        row: dict[str, object] = {
            "type": self.type.decode("latin-1"),
            "offset": self.offset,
            "size": self.size,
            "header_size": self.header_size,
            "depth": self.depth,
        }
        if self.version is not None:
            row["version"] = self.version
            row["flags"] = self.flags
        if self.children:
            row["children"] = len(self.children)
        return row


def parse_boxes(data: bytes, *, start: int = 0, end: int | None = None, depth: int = 0, max_depth: int = 12) -> list[BoxRecord]:
    """Read the box sequence in ``data[start:end]``, descending into containers."""
    limit = len(data) if end is None else end
    if depth > max_depth:
        raise Mp4Error("box_depth_limit", f"box nesting deeper than {max_depth}", start)
    records: list[BoxRecord] = []
    cursor = start
    while cursor < limit:
        if limit - cursor < 8:
            raise Mp4Error("box_truncated", f"{limit - cursor} trailing bytes cannot form a box", cursor)
        size, box_type = struct.unpack(">I4s", data[cursor : cursor + 8])
        header = 8
        if size == 1:
            if limit - cursor < 16:
                raise Mp4Error("box_truncated", "largesize box header truncated", cursor)
            size = struct.unpack(">Q", data[cursor + 8 : cursor + 16])[0]
            header = 16
        elif size == 0:
            size = limit - cursor
        if size < header:
            raise Mp4Error("box_size_invalid", f"{box_type!r} size {size} below header", cursor)
        if cursor + size > limit:
            raise Mp4Error(
                "box_overrun", f"{box_type!r} at {cursor} ends past container end {limit}", cursor
            )
        record = BoxRecord(type=box_type, offset=cursor, header_size=header, size=size, depth=depth)
        if box_type == b"stsd" and size >= header + 8:
            record.children = _parse_sample_entries(data, cursor + header + 8, cursor + size, depth + 1)
        elif box_type == b"dref" and size >= header + 8:
            record.children = parse_boxes(
                data, start=cursor + header + 8, end=cursor + size, depth=depth + 1, max_depth=max_depth
            )
        elif box_type in CONTAINER_TYPES and size > header:
            record.children = parse_boxes(
                data, start=cursor + header, end=cursor + size, depth=depth + 1, max_depth=max_depth
            )
        elif box_type in FULL_BOX_TYPES and size >= header + 4:
            version = data[cursor + header]
            flags = int.from_bytes(data[cursor + header + 1 : cursor + header + 4], "big")
            record.version = version
            record.flags = flags
        elif box_type in SAMPLE_ENTRY_TYPES:
            fixed = SAMPLE_ENTRY_FIXED.get(box_type, 0)
            child_start = cursor + header + fixed
            if child_start < cursor + size:
                record.children = parse_boxes(
                    data, start=child_start, end=cursor + size, depth=depth + 1, max_depth=max_depth
                )
        records.append(record)
        cursor += size
    return records


def _parse_sample_entries(data: bytes, start: int, end: int, depth: int) -> list[BoxRecord]:
    """Parse the entry boxes inside an stsd box."""
    records: list[BoxRecord] = []
    cursor = start
    while cursor < end:
        if end - cursor < 8:
            raise Mp4Error("box_truncated", "sample entry header truncated", cursor)
        size, box_type = struct.unpack(">I4s", data[cursor : cursor + 8])
        if size < 8 or cursor + size > end:
            raise Mp4Error("box_size_invalid", f"sample entry {box_type!r} size {size} invalid", cursor)
        record = BoxRecord(type=box_type, offset=cursor, header_size=8, size=size, depth=depth)
        fixed = SAMPLE_ENTRY_FIXED.get(box_type, 0)
        child_start = cursor + 8 + fixed
        if child_start < cursor + size:
            record.children = parse_boxes(
                data, start=child_start, end=cursor + size, depth=depth + 1
            )
        records.append(record)
        cursor += size
    return records


def assign_offsets(root: Box, start: int, *, depth: int = 0) -> list[BoxRecord]:
    """Walk a box tree assigning file offsets, mirroring :func:`parse_boxes`.

    Returns the root record only; descendants hang off ``.children`` exactly
    like the parser's output, so callers can compare trees node for node.
    """

    def emit(current: Box, offset: int, level: int) -> BoxRecord:
        body = current.body()
        total = 8 + len(body)
        header = 8
        if total > 0xFFFFFFFF:
            total = 16 + len(body)
            header = 16
        record = BoxRecord(
            type=current.type,
            offset=offset,
            header_size=header,
            size=total,
            version=current.version,
            flags=current.flags,
            depth=level,
        )
        child_cursor = offset + header
        if current.version is not None and current.type not in CONTAINER_TYPES:
            child_cursor += 4  # FullBox version + flags
        if current.type in (b"stsd", b"dref"):
            child_cursor += 4  # entry_count, after the FullBox header
        elif current.type in SAMPLE_ENTRY_TYPES:
            child_cursor += SAMPLE_ENTRY_FIXED.get(current.type, 0)
        for child in current.children:
            child_record = emit(child, child_cursor, level + 1)
            record.children.append(child_record)
            child_cursor += child_record.size
        return record

    return [emit(root, start, depth)]


__all__ = ["BoxRecord", "assign_offsets", "parse_boxes"]
