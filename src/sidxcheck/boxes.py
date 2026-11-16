"""Bounded ISO BMFF box parsing with top-level resynchronisation."""

from __future__ import annotations

import mmap
import os
import struct
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator, Sequence

DEFAULT_MAX_DEPTH = 32
DEFAULT_MAX_BOX_SIZE = 1 << 30
MAX_FILE_SIZE = 1 << 34
RESYNC_WINDOW = 16 << 20

_CONTAINERS = {
    "moov", "trak", "mdia", "minf", "stbl", "moof", "traf", "mvex",
    "mfra", "dinf", "edts", "udta", "meta",
}
_FULLBOX_CONTAINERS = {"meta": 4}
_RESYNC_TYPES = {
    "ftyp", "styp", "moov", "moof", "mdat", "sidx", "mfra", "mfro",
    "free", "skip", "wide", "emsg", "prft", "uuid",
}


@dataclass(slots=True)
class ParseIssue:
    offset: int
    code: str
    message: str
    severity: str = "fatal"
    box_type: str | None = None

    def as_dict(self) -> dict[str, object]:
        return {
            "offset": self.offset,
            "code": self.code,
            "message": self.message,
            "severity": self.severity,
            "box_type": self.box_type,
        }


@dataclass(slots=True)
class Box:
    type: str
    start: int
    size: int
    header_size: int
    payload_start: int
    end: int
    depth: int
    uuid: bytes | None = None
    children: list["Box"] = field(default_factory=list)

    @property
    def payload_size(self) -> int:
        return self.end - self.payload_start


@dataclass(slots=True)
class ParsedFile:
    path: Path
    data: memoryview
    boxes: list[Box]
    issues: list[ParseIssue]
    _mapping: mmap.mmap | None = None

    def close(self) -> None:
        if self.data is not None:
            self.data.release()
        if self._mapping is not None:
            self._mapping.close()
            self._mapping = None

    def __enter__(self) -> "ParsedFile":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


@dataclass(slots=True)
class _Header:
    type: str
    start: int
    size: int
    header_size: int
    payload_start: int
    end: int
    uuid: bytes | None


def parse_bytes(
    data: bytes | bytearray | memoryview,
    *,
    max_depth: int = DEFAULT_MAX_DEPTH,
    max_box_size: int = DEFAULT_MAX_BOX_SIZE,
) -> tuple[list[Box], list[ParseIssue]]:
    issues: list[ParseIssue] = []
    view = memoryview(data)
    boxes = _parse_top_level(view, len(view), max_depth, max_box_size, issues)
    return boxes, issues


def parse_file(
    path: str | Path,
    *,
    max_depth: int = DEFAULT_MAX_DEPTH,
    max_box_size: int = DEFAULT_MAX_BOX_SIZE,
) -> ParsedFile:
    source = Path(path)
    issues: list[ParseIssue] = []
    stat = os.stat(source)
    if stat.st_size > MAX_FILE_SIZE:
        issue = ParseIssue(0, "file_size_limit", f"file exceeds {MAX_FILE_SIZE} bytes")
        return ParsedFile(source, memoryview(b""), [], [issue])
    if stat.st_size == 0:
        return ParsedFile(source, memoryview(b""), [], [])

    handle = open(source, "rb")
    try:
        mapping = mmap.mmap(handle.fileno(), 0, access=mmap.ACCESS_READ)
    finally:
        handle.close()
    view = memoryview(mapping)
    boxes = _parse_top_level(view, len(view), max_depth, max_box_size, issues)
    return ParsedFile(source, view, boxes, issues, mapping)


def walk_boxes(boxes: Sequence[Box]) -> Iterator[Box]:
    for box in boxes:
        yield box
        yield from walk_boxes(box.children)


def find_boxes(boxes: Sequence[Box], *types: str) -> list[Box]:
    wanted = set(types)
    return [box for box in walk_boxes(boxes) if box.type in wanted]


def _parse_top_level(
    data: memoryview,
    limit: int,
    max_depth: int,
    max_box_size: int,
    issues: list[ParseIssue],
) -> list[Box]:
    boxes: list[Box] = []
    pos = 0
    while pos < limit:
        if limit - pos < 8:
            issues.append(ParseIssue(pos, "truncated_header", "incomplete top-level box header"))
            break
        header, issue = _read_header(data, pos, limit, max_box_size)
        if header is None:
            assert issue is not None
            issues.append(issue)
            next_pos = _resync(data, pos + 1, limit, max_box_size)
            if next_pos is None:
                break
            pos = next_pos
            continue
        box = _make_box(data, header, 0, max_depth, max_box_size, issues)
        boxes.append(box)
        pos = box.end
    return boxes


def _parse_children(
    data: memoryview,
    start: int,
    limit: int,
    depth: int,
    max_depth: int,
    max_box_size: int,
    issues: list[ParseIssue],
) -> list[Box]:
    if depth > max_depth:
        issues.append(ParseIssue(start, "max_depth", f"box nesting exceeds depth {max_depth}"))
        return []
    children: list[Box] = []
    pos = start
    while pos < limit:
        if limit - pos < 8:
            issues.append(ParseIssue(pos, "truncated_header", "incomplete child box header"))
            break
        header, issue = _read_header(data, pos, limit, max_box_size)
        if header is None:
            assert issue is not None
            issues.append(issue)
            break
        box = _make_box(data, header, depth, max_depth, max_box_size, issues)
        children.append(box)
        pos = box.end
    return children


def _make_box(
    data: memoryview,
    header: _Header,
    depth: int,
    max_depth: int,
    max_box_size: int,
    issues: list[ParseIssue],
) -> Box:
    box = Box(
        type=header.type,
        start=header.start,
        size=header.size,
        header_size=header.header_size,
        payload_start=header.payload_start,
        end=header.end,
        depth=depth,
        uuid=header.uuid,
    )
    if header.type in _CONTAINERS:
        prefix = _FULLBOX_CONTAINERS.get(header.type, 0)
        child_start = header.payload_start + prefix
        if child_start <= header.end:
            box.children = _parse_children(
                data, child_start, header.end, depth + 1,
                max_depth, max_box_size, issues,
            )
    return box


def _read_header(
    data: memoryview,
    offset: int,
    limit: int,
    max_box_size: int,
) -> tuple[_Header | None, ParseIssue | None]:
    if limit - offset < 8:
        return None, ParseIssue(offset, "truncated_header", "box header shorter than 8 bytes")
    size32 = struct.unpack_from(">I", data, offset)[0]
    fourcc = bytes(data[offset + 4:offset + 8])
    box_type = fourcc.decode("latin-1")
    header_size = 8
    size = size32
    if size32 == 1:
        if limit - offset < 16:
            return None, ParseIssue(offset, "truncated_largesize", "missing 64-bit largesize", box_type=box_type)
        size = struct.unpack_from(">Q", data, offset + 8)[0]
        header_size = 16
    if box_type == "uuid":
        header_size += 16
        if limit - offset < header_size:
            return None, ParseIssue(offset, "truncated_uuid", "missing uuid user type", box_type=box_type)
    if size32 == 0:
        size = limit - offset
    if size < header_size:
        return None, ParseIssue(offset, "invalid_size", f"box size {size} is below header size {header_size}", box_type=box_type)
    if size > max_box_size:
        return None, ParseIssue(offset, "box_size_limit", f"box size {size} exceeds limit {max_box_size}", box_type=box_type)
    end = offset + size
    if end > limit:
        return None, ParseIssue(offset, "truncated_box", f"box ends at {end}, limit is {limit}", box_type=box_type)
    uuid = bytes(data[offset + header_size - 16:offset + header_size]) if box_type == "uuid" else None
    return _Header(box_type, offset, size, header_size, offset + header_size, end, uuid), None


def _resync(data: memoryview, start: int, limit: int, max_box_size: int) -> int | None:
    stop = min(limit - 8, start + RESYNC_WINDOW)
    pos = start
    while pos <= stop:
        candidate = bytes(data[pos + 4:pos + 8]).decode("latin-1")
        if candidate in _RESYNC_TYPES:
            header, _ = _read_header(data, pos, limit, max_box_size)
            if header is not None and header.type in _RESYNC_TYPES:
                return pos
        pos += 1
    return None
