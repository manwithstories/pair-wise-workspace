"""Build an offset-aware Matroska element tree from the EBML reader."""

from __future__ import annotations

import os
import struct
from dataclasses import dataclass, field

from mkvtomp4.ebml.reader import (
    DepthLimitError,
    EBMLReadError,
    EBMLReader,
    ElementHeader,
    TruncatedFileError,
)
from mkvtomp4.ebml.schema import (
    CHILDREN,
    CLUSTER_ID,
    CRC_ID,
    EBML_ID,
    MASTER_IDS,
    SEGMENT_CHILDREN,
    SEGMENT_ID,
    ElementSpec,
    hex_id,
    spec_for,
)

INLINE_LIMIT = 4096
NON_INLINE_BLOCK_IDS = frozenset({0xA3, 0xA1})


@dataclass
class ElementNode:
    eid: int
    name: str
    kind: str
    offset: int
    header_size: int
    data_offset: int
    size: int | None
    depth: int
    was_unknown: bool = False
    parent: "ElementNode | None" = None
    children: list["ElementNode"] = field(default_factory=list)
    payload: bytes | None = None

    @property
    def end(self) -> int | None:
        return None if self.size is None else self.data_offset + self.size

    @property
    def unknown_size(self) -> bool:
        return self.size is None

    def find_all(self, eid: int) -> list["ElementNode"]:
        found: list[ElementNode] = []
        stack = list(reversed(self.children))
        while stack:
            node = stack.pop()
            if node.eid == eid:
                found.append(node)
            stack.extend(reversed(node.children))
        return found

    def first(self, eid: int) -> "ElementNode | None":
        for node in self.find_all(eid):
            return node
        return None


@dataclass
class ElementSummary:
    element_count: int
    max_depth: int
    unknown_size_count: int
    cue_entries: int
    segment_count: int
    first_elements: list[dict[str, object]]
    id_counts: dict[str, int]


class MatroskaTree:
    def __init__(self, path: os.PathLike[str] | str, *, max_depth: int = 16):
        self.path = os.fspath(path)
        self.max_depth_limit = max_depth
        self.reader = EBMLReader.from_path(self.path)
        self.roots: list[ElementNode] = []
        self.element_count = 0
        self.max_depth = 0
        self.unknown_size_count = 0
        self.id_counts: dict[str, int] = {}
        self._parse()

    @property
    def warnings(self) -> list[dict[str, object]]:
        return self.reader.warnings

    def _parse(self) -> None:
        while self.reader.tell() < self.reader.size:
            self._parse_element(depth=1, parent=None)

    def _record(self, node: ElementNode) -> None:
        self.element_count += 1
        self.max_depth = max(self.max_depth, node.depth)
        if node.was_unknown:
            self.unknown_size_count += 1
        key = hex_id(node.eid)
        self.id_counts[key] = self.id_counts.get(key, 0) + 1

    def _parse_element(self, *, depth: int, parent: ElementNode | None) -> ElementNode:
        if depth > self.max_depth_limit:
            raise DepthLimitError(f"EBML nesting exceeds depth {self.max_depth_limit}")
        header = self.reader.read_header()
        spec = spec_for(header.eid)
        node = ElementNode(
            header.eid,
            spec.name,
            spec.kind,
            header.offset,
            header.header_size,
            header.data_offset,
            header.size,
            depth,
            header.unknown_size,
            parent,
        )
        if parent is not None:
            parent.children.append(node)
        else:
            self.roots.append(node)
        self._record(node)
        if header.eid in MASTER_IDS:
            if header.unknown_size:
                self._parse_unknown_master(node)
            else:
                self._parse_known_master(node, header)
        else:
            if header.size is None:
                raise EBMLReadError(
                    f"unknown-size non-master element 0x{header.eid:X} at {header.offset}"
                )
            node.payload = self._read_leaf(node)
        return node

    def _read_leaf(self, node: ElementNode) -> bytes | None:
        if node.size is None or node.size > INLINE_LIMIT:
            self.reader.seek(node.data_offset + (node.size or 0))
            return None
        if node.eid in NON_INLINE_BLOCK_IDS:
            self.reader.seek(node.data_offset + node.size)
            return None
        data = self.reader.read_range(node.data_offset, node.size)
        self.reader.seek(node.data_offset + node.size)
        return data

    def _parse_known_master(self, node: ElementNode, header: ElementHeader) -> None:
        end = header.end
        assert end is not None
        while self.reader.tell() < end:
            child = self._parse_element(depth=node.depth + 1, parent=node)
            if child.end is None or child.end > end:
                raise EBMLReadError(
                    f"child 0x{child.eid:X} overruns parent {node.name}"
                )
        if self.reader.tell() != end:
            raise EBMLReadError(f"master {node.name} end mismatch at {header.offset}")
        self._verify_crc_child(node, end)

    def _parse_unknown_master(self, node: ElementNode) -> None:
        start = node.data_offset
        while self.reader.tell() < self.reader.size:
            position = self.reader.tell()
            header = self.reader.read_header()
            if not self._could_be_child(node.eid, header.eid):
                self.reader.seek(position)
                break
            self.reader.seek(position)
            self._parse_element(depth=node.depth + 1, parent=node)
        end = self.reader.tell()
        if end == start and self.reader.size - start < 2:
            raise TruncatedFileError(f"empty unknown-size master {node.name}")
        node.size = end - start
        self._verify_crc_child(node, end)

    def _could_be_child(self, parent_eid: int, child_eid: int) -> bool:
        allowed = CHILDREN.get(parent_eid)
        if allowed is not None and child_eid in allowed:
            return True
        if parent_eid == SEGMENT_ID:
            return False
        if parent_eid == CLUSTER_ID:
            return child_eid not in SEGMENT_CHILDREN | {SEGMENT_ID, EBML_ID}
        if allowed is not None:
            return False
        return True

    def _verify_crc_child(self, node: ElementNode, master_end: int) -> None:
        crc_nodes = [child for child in node.children if child.eid == CRC_ID]
        if not crc_nodes:
            return
        crc = crc_nodes[0]
        if crc.size != 4 or crc.payload is None:
            self.reader.warnings.append(
                {
                    "kind": "crc",
                    "parent": node.name,
                    "offset": crc.offset,
                    "message": "invalid CRC-32 payload size",
                }
            )
            return
        self.reader.verify_crc(
            crc_element_offset=crc.offset,
            crc_data_offset=crc.data_offset,
            master_end=master_end,
            parent_name=node.name,
        )

    def segments(self) -> list[ElementNode]:
        return [root for root in self.roots if root.eid == SEGMENT_ID]

    def segment_children(self) -> list[ElementNode]:
        segment = next((root for root in self.roots if root.eid == SEGMENT_ID), None)
        return [] if segment is None else list(segment.children)

    def seekheads(self) -> list[ElementNode]:
        return [node for segment in self.segments() for node in segment.children if node.eid == 0x114D9B74]

    def cues(self) -> list[ElementNode]:
        return [node for segment in self.segments() for node in segment.children if node.eid == 0x1C53BB6B]

    def cue_entries(self) -> int:
        total = 0
        for cues in self.cues():
            total += len(cues.find_all(0xBB))
        return total

    def summary(self) -> ElementSummary:
        children = self.segment_children()
        first = children[:20] if children else self.roots[:20]
        offsets = [
            {
                "id": hex_id(node.eid),
                "name": node.name,
                "offset": node.offset,
                "size": node.size,
                "unknown_size": node.was_unknown,
            }
            for node in first
        ]
        return ElementSummary(
            element_count=self.element_count,
            max_depth=self.max_depth,
            unknown_size_count=self.unknown_size_count,
            cue_entries=self.cue_entries(),
            segment_count=len(self.segments()),
            first_elements=offsets,
            id_counts=dict(sorted(self.id_counts.items())),
        )


def payload_bytes(tree: MatroskaTree, node: ElementNode) -> bytes:
    if node.payload is not None:
        return node.payload
    if node.size is None:
        raise EBMLReadError(f"element {node.name} has no known payload size")
    return tree.reader.read_range(node.data_offset, node.size)


def uint_value(tree: MatroskaTree, node: ElementNode | None, default: int = 0) -> int:
    if node is None or node.size is None:
        return default
    data = payload_bytes(tree, node)
    return int.from_bytes(data, "big", signed=False) if data else default


def int_value(tree: MatroskaTree, node: ElementNode | None, default: int = 0) -> int:
    if node is None or node.size is None:
        return default
    data = payload_bytes(tree, node)
    return int.from_bytes(data, "big", signed=True) if data else default


def float_value(tree: MatroskaTree, node: ElementNode | None, default: float = 0.0) -> float:
    if node is None or node.size not in (4, 8):
        return default
    data = payload_bytes(tree, node)
    return struct.unpack(">f" if node.size == 4 else ">d", data)[0]


def string_value(tree: MatroskaTree, node: ElementNode | None, default: str = "") -> str:
    if node is None or node.size is None:
        return default
    data = payload_bytes(tree, node).rstrip(b"\x00")
    return data.decode("utf-8", errors="replace") if data else default


def first_descendant(node: ElementNode | None, eid: int) -> ElementNode | None:
    return None if node is None else node.first(eid)
