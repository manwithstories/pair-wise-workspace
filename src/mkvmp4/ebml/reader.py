"""Buffered, seekable EBML reader with unknown-size group support."""

from __future__ import annotations

import io
import os
import struct
import zlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO, Callable

from .ids import CONTAINERS, CRC32, SEGMENT, SEGMENT_CHILDREN, element_name
from .vint import VINTError, read_element_id, read_vint


class EBMLDecodeError(ValueError):
    pass


@dataclass(slots=True)
class ElementHeader:
    element_id: int
    offset: int
    header_size: int
    size: int | None
    unknown: bool
    depth: int = 0

    @property
    def name(self) -> str:
        return element_name(self.element_id)

    @property
    def data_offset(self) -> int:
        return self.offset + self.header_size

    @property
    def end(self) -> int | None:
        return None if self.unknown else self.data_offset + (self.size or 0)

    @property
    def length_text(self) -> str | int:
        return "unknown" if self.unknown else (self.size or 0)


@dataclass(slots=True)
class ElementNode:
    element_id: int
    offset: int
    header_size: int
    size: int | None
    unknown: bool
    depth: int
    children: list["ElementNode"] = field(default_factory=list)

    @property
    def name(self) -> str:
        return element_name(self.element_id)


@dataclass(slots=True)
class WalkStats:
    elements: int = 0
    unknown_size: int = 0
    clusters: int = 0
    nodes: list[ElementNode] = field(default_factory=list)


class EBMLReader:
    """Reads EBML headers without materialising element payloads."""

    def __init__(self, source: str | os.PathLike[str] | BinaryIO, *, validate_crc: bool = True):
        if hasattr(source, "read"):
            self._stream = source  # type: ignore[assignment]
            self._owns_stream = False
            self.path = getattr(source, "name", None)
        else:
            self.path = os.fspath(source)
            self._stream = open(self.path, "rb")
            self._owns_stream = True
        self._buf = io.BufferedReader(self._stream, buffer_size=256 * 1024)
        self.validate_crc = validate_crc
        self.size = self._find_size()

    def _find_size(self) -> int:
        pos = self._buf.tell()
        self._buf.seek(0, os.SEEK_END)
        size = self._buf.tell()
        self._buf.seek(pos)
        return size

    def close(self) -> None:
        if self._owns_stream:
            self._buf.close()
        else:
            self._buf.detach()

    def __enter__(self) -> "EBMLReader":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    @property
    def tell(self) -> int:
        return self._buf.tell()

    def seek(self, offset: int) -> None:
        self._buf.seek(offset)

    def read(self, size: int) -> bytes:
        data = self._buf.read(size)
        if len(data) != size:
            raise EBMLDecodeError("unexpected EOF")
        return data

    def read_header(self, depth: int = 0) -> ElementHeader:
        offset = self._buf.tell()
        try:
            element_id, id_width = read_element_id(self._buf)
            size, size_width, unknown = read_vint(self._buf)
        except VINTError as exc:
            raise EBMLDecodeError(str(exc)) from exc
        header = ElementHeader(element_id, offset, id_width + size_width,
                               None if unknown else size, unknown, depth)
        if header.end is not None and header.end > self.size:
            raise EBMLDecodeError(
                f"element {header.name} at {offset} extends beyond EOF")
        return header

    def read_payload(self, header: ElementHeader) -> bytes:
        if header.unknown:
            raise EBMLDecodeError(f"unknown-size {header.name} has no payload bound")
        self.seek(header.data_offset)
        return self.read(header.size or 0)

    def read_payload_uint(self, header: ElementHeader) -> int:
        data = self.read_payload(header)
        return int.from_bytes(data, "big")

    def read_payload_int(self, header: ElementHeader) -> int:
        data = self.read_payload(header)
        return int.from_bytes(data, "big", signed=True)

    def read_payload_float(self, header: ElementHeader) -> float:
        data = self.read_payload(header)
        if len(data) == 4:
            return struct.unpack(">f", data)[0]
        if len(data) == 8:
            return struct.unpack(">d", data)[0]
        if not data:
            return 0.0
        raise EBMLDecodeError(f"invalid float width {len(data)}")

    def read_payload_string(self, header: ElementHeader) -> str:
        return self.read_payload(header).decode("utf-8", "replace").rstrip("\x00")

    def skip(self, count: int) -> None:
        if count < 0:
            raise EBMLDecodeError("cannot skip backwards")
        target = self._buf.tell() + count
        if target > self.size:
            raise EBMLDecodeError("skip past EOF")
        self._buf.seek(target)

    def _peek_id(self) -> int | None:
        pos = self._buf.tell()
        first = self._buf.read(1)
        self._buf.seek(pos)
        if not first:
            return None
        from .vint import vint_length

        width = vint_length(first[0])
        raw = self._buf.read(width)
        self._buf.seek(pos)
        if len(raw) != width:
            raise EBMLDecodeError("truncated element ID")
        return int.from_bytes(raw, "big")

    def _boundary_for(self, header: ElementHeader) -> frozenset[int]:
        if header.element_id == SEGMENT:
            return frozenset()
        return SEGMENT_CHILDREN

    def walk(self, on_element: Callable[[ElementHeader], None] | None = None,
             *, max_depth: int | None = None,
             collect_nodes: bool = False) -> WalkStats:
        """Walk all elements, descending known containers.

        Unknown-size containers are terminated by the next sibling in their
        parent's boundary set.  In particular Segment runs to EOF and Cluster
        ends at Cluster/Cues/Chapters/Tags/SeekHead/Info/Tracks.
        """
        stats = WalkStats()
        node_stack: list[ElementNode] = []
        self.seek(0)
        self._walk_level(0, self.size, frozenset(), 0, on_element, max_depth,
                         collect_nodes, stats, node_stack)
        if node_stack:
            raise EBMLDecodeError("unclosed container")
        return stats

    def _walk_level(self, start: int, end: int | None, boundary: frozenset[int],
                    depth: int, on_element: Callable[[ElementHeader], None] | None,
                    max_depth: int | None, collect_nodes: bool,
                    stats: WalkStats, node_stack: list[ElementNode]) -> int:
        pos = start
        crc_header: ElementHeader | None = None
        self.seek(pos)
        while end is None or pos < end:
            if pos == self.size:
                if end is None:
                    break
                raise EBMLDecodeError("container ended before its declared size")
            if boundary:
                peeked = self._peek_id()
                if peeked is None:
                    if end is None:
                        break
                    raise EBMLDecodeError("unexpected EOF in unknown-size container")
                if peeked in boundary:
                    break
            header = self.read_header(depth)
            if end is not None and header.end is not None and header.end > end:
                raise EBMLDecodeError(f"{header.name} overlaps parent boundary")
            if depth > 64:
                raise EBMLDecodeError("EBML nesting exceeds safety limit")
            stats.elements += 1
            stats.unknown_size += int(header.unknown)
            if header.element_id == 0x1F43B675:
                stats.clusters += 1
            if on_element is not None:
                on_element(header)
            node = None
            if collect_nodes and (max_depth is None or depth <= max_depth):
                node = ElementNode(header.element_id, header.offset, header.header_size,
                                   header.size, header.unknown, depth)
                if node_stack:
                    node_stack[-1].children.append(node)
                else:
                    stats.nodes.append(node)
            if header.element_id == CRC32 and self.validate_crc:
                # The checksum covers the remainder of this parent element.
                crc_header = header
            descend = header.element_id in CONTAINERS
            if descend:
                if collect_nodes and (max_depth is None or depth <= max_depth):
                    node_stack.append(node)  # type: ignore[arg-type]
                child_end = header.end
                child_boundary = self._boundary_for(header) if header.unknown else frozenset()
                new_end = self._walk_level(header.data_offset, child_end, child_boundary,
                                           depth + 1, on_element, max_depth,
                                           collect_nodes, stats, node_stack)
                if collect_nodes and (max_depth is None or depth <= max_depth):
                    node_stack.pop()
                if header.unknown:
                    pos = new_end
                else:
                    pos = header.end or header.data_offset
                self.seek(pos)
            else:
                if header.unknown:
                    raise EBMLDecodeError(f"unknown-size leaf {header.name}")
                pos = header.end or header.data_offset
                self.seek(pos)
        actual_end = end if end is not None else pos
        if crc_header is not None:
            self.crc_check(crc_header, actual_end)
            self.seek(pos)
        return pos

    def crc_check(self, header: ElementHeader, end: int) -> None:
        expected_bytes = self.read_payload(header)
        if len(expected_bytes) != 4:
            raise EBMLDecodeError("CRC-32 payload must be four bytes")
        expected = int.from_bytes(expected_bytes, "big")
        actual = 0
        self.seek(header.end or header.data_offset)
        remaining = end - (header.end or header.data_offset)
        while remaining:
            chunk = self._buf.read(min(1024 * 1024, remaining))
            if not chunk:
                raise EBMLDecodeError("EOF while validating CRC-32")
            actual = zlib.crc32(chunk, actual)
            remaining -= len(chunk)
        if actual & 0xFFFFFFFF != expected:
            raise EBMLDecodeError("CRC-32 mismatch")
        self.seek(end)

