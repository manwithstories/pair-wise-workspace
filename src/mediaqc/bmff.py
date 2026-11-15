"""Small, defensive ISO BMFF box reader used by the probe pipeline."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from pathlib import Path

from .model import Finding

MAX_DEPTH = 32
MAX_METADATA_BYTES = 32 * 1024 * 1024
MAX_BOXES = 200_000
CONTAINER_TYPES = {
    b"moov", b"trak", b"mdia", b"minf", b"stbl", b"edts", b"dinf",
    b"mvex", b"moof", b"traf", b"mfra", b"udta", b"ilst", b"wave",
}


class BoxError(ValueError):
    def __init__(self, message: str, offset: int) -> None:
        super().__init__(message)
        self.offset = offset


@dataclass(slots=True)
class Box:
    type: bytes
    offset: int
    size: int
    header_size: int
    end: int
    depth: int = 0

    @property
    def payload_offset(self) -> int:
        return self.offset + self.header_size

    @property
    def payload_size(self) -> int:
        return self.size - self.header_size

    @property
    def name(self) -> str:
        return self.type.decode("latin-1", "replace")

    def __contains__(self, child: "Box") -> bool:
        return self.payload_offset <= child.offset and child.end <= self.end


@dataclass(slots=True)
class BmffScan:
    boxes: list[Box] = field(default_factory=list)
    mdat_ranges: list[tuple[int, int]] = field(default_factory=list)
    findings: list[Finding] = field(default_factory=list)
    has_moof: bool = False
    unsupported_layout: bool = False
    trailing_bytes: int = 0

    def top(self, name: str) -> Box | None:
        looking = name.encode("latin-1")
        return next((box for box in self.boxes if box.type == looking), None)


class BoxReader:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.size = self.path.stat().st_size

    def scan(self) -> BmffScan:
        result = BmffScan()
        with self.path.open("rb") as handle:
            try:
                self._scan_level(handle, 0, self.size, 0, result)
            except BoxError as exc:
                result.findings.append(Finding(
                    "BOX_BOUNDARY_INVALID", "error", "box length escapes its parent",
                    str(exc), offset=exc.offset,
                ))
        if result.has_moof:
            result.unsupported_layout = True
            first = next(box for box in result.boxes if box.type == b"moof")
            result.findings.append(Finding(
                "UNSUPPORTED_LAYOUT", "error", "fragmented MP4/MOV (moof) is unsupported",
                "top-level moof present", offset=first.offset,
            ))
        return result

    def _scan_level(self, handle, start: int, end: int, depth: int, result: BmffScan) -> None:
        if depth > MAX_DEPTH:
            raise BoxError("box nesting exceeds 32 levels", start)
        offset = start
        while offset < end:
            remaining = end - offset
            if remaining < 8:
                if depth == 0:
                    result.trailing_bytes = remaining
                    result.findings.append(Finding(
                        "TAIL_GARBAGE", "warning", "trailing bytes do not form a box",
                        f"{remaining} trailing byte(s)", offset=offset,
                    ))
                else:
                    result.findings.append(Finding(
                        "BOX_BOUNDARY_INVALID", "error", "trailing bytes inside container box",
                        f"{remaining} trailing byte(s) at depth {depth}", offset=offset,
                    ))
                return
            box = self._read_header(handle, offset, end, depth)
            if len(result.boxes) >= MAX_BOXES:
                raise BoxError("box count exceeds memory budget", offset)
            result.boxes.append(box)
            if box.type == b"mdat":
                result.mdat_ranges.append((box.payload_offset, box.end))
            if box.type == b"moof":
                result.has_moof = True
            if box.type in CONTAINER_TYPES:
                if depth >= MAX_DEPTH:
                    raise BoxError("box nesting exceeds 32 levels", box.offset)
                self._scan_level(handle, box.payload_offset, box.end, depth + 1, result)
            offset = box.end
        if depth == 0 and result.findings:
            for item in result.findings:
                if item.rule_id == "BOX_BOUNDARY_INVALID":
                    break

    def _read_header(self, handle, offset: int, parent_end: int, depth: int) -> Box:
        handle.seek(offset)
        header = handle.read(8)
        if len(header) != 8:
            raise BoxError("truncated box header", offset)
        size32, box_type = struct.unpack(">I4s", header)
        header_size = 8
        if size32 == 1:
            extended = handle.read(8)
            if len(extended) != 8:
                raise BoxError("truncated 64-bit box length", offset)
            box_size = struct.unpack(">Q", extended)[0]
            header_size = 16
        elif size32 == 0:
            box_size = parent_end - offset
        else:
            box_size = size32
        if box_size < header_size:
            raise BoxError(f"box {box_type!r} is smaller than its header", offset)
        if offset + box_size > parent_end:
            raise BoxError(
                f"box {box_type!r} ends at {offset + box_size}, parent ends at {parent_end}",
                offset,
            )
        return Box(box_type, offset, box_size, header_size, offset + box_size, depth)


def read_payload(path: str | Path, box: Box, limit: int = MAX_METADATA_BYTES) -> bytes:
    length = min(box.payload_size, limit)
    with Path(path).open("rb") as handle:
        handle.seek(box.payload_offset)
        data = handle.read(length)
    if len(data) != length:
        raise BoxError("short read inside metadata box", box.offset)
    return data


def child_boxes(handle, parent: Box) -> list[Box]:
    boxes: list[Box] = []
    offset = parent.payload_offset
    while offset < parent.end:
        if parent.end - offset < 8:
            raise BoxError("garbage inside container box", offset)
        handle.seek(offset)
        size32, kind = struct.unpack(">I4s", handle.read(8))
        header = 8
        if size32 == 1:
            ext = handle.read(8)
            if len(ext) != 8:
                raise BoxError("truncated child box length", offset)
            size = struct.unpack(">Q", ext)[0]
            header = 16
        elif size32 == 0:
            size = parent.end - offset
        else:
            size = size32
        if size < header or offset + size > parent.end:
            raise BoxError("child box escapes container", offset)
        boxes.append(Box(kind, offset, size, header, offset + size, parent.depth + 1))
        offset += size
    return boxes


def payload(handle, box: Box, limit: int = MAX_METADATA_BYTES) -> bytes:
    if box.payload_size > limit:
        raise BoxError("metadata payload exceeds memory limit", box.offset)
    handle.seek(box.payload_offset)
    data = handle.read(box.payload_size)
    if len(data) != box.payload_size:
        raise BoxError("short metadata payload", box.offset)
    return data
