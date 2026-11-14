"""Build an EBML element tree, including unknown-size masters."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field

from mkvtool.ebml.schema import (
    BINARY,
    EBML_ID,
    FLOAT,
    INT,
    MASTER,
    SEGMENT_ID,
    STRING,
    UINT,
    UTF8,
    can_contain,
    element_name,
    get_spec,
    is_master,
)
from mkvtool.ebml.vint import ElementHeader, encode_element_id, parse_element_header
from mkvtool.errors import InputParseError


@dataclass
class Element:
    element_id: int
    name: str
    type: str
    offset: int
    data_offset: int
    size: int | None
    end_offset: int
    unknown_size: bool
    depth: int
    header_length: int
    children: list["Element"] = field(default_factory=list)
    raw: bytes = b""
    boundary_reason: str = "declared_size"
    truncated: bool = False
    parent: "Element | None" = field(default=None, repr=False)

    @property
    def data_size(self) -> int:
        if self.size is not None:
            return self.size
        return max(0, self.end_offset - self.data_offset)

    @property
    def value(self):
        if self.type == UINT:
            return int.from_bytes(self.raw, "big") if self.raw else 0
        if self.type == INT:
            return int.from_bytes(self.raw, "big", signed=True) if self.raw else 0
        if self.type == FLOAT:
            if len(self.raw) == 4:
                return struct.unpack(">f", self.raw)[0]
            if len(self.raw) == 8:
                return struct.unpack(">d", self.raw)[0]
            raise InputParseError(f"{self.name} has invalid float width {len(self.raw)}")
        if self.type in (STRING, UTF8):
            return self.raw.rstrip(b"\x00").decode("utf-8", errors="replace")
        return self.raw

    def child(self, element_id: int) -> "Element | None":
        for item in self.children:
            if item.element_id == element_id:
                return item
        return None

    def children_of(self, element_id: int) -> list["Element"]:
        return [item for item in self.children if item.element_id == element_id]

    def child_value(self, element_id: int, default=None):
        item = self.child(element_id)
        return default if item is None else item.value

    def walk(self):
        yield self
        for item in self.children:
            yield from item.walk()

    @property
    def path(self) -> str:
        if self.parent is None:
            return f"/{self.name}"
        same = [x for x in self.parent.children if x.element_id == self.element_id]
        index = same.index(self)
        return f"{self.parent.path}/{self.name}[{index}]"


@dataclass
class ParseResult:
    data: bytes
    roots: list[Element]
    issues: list[dict]
    truncated: bool = False
    truncation_offset: int | None = None
    document: Element | None = None

    def walk(self):
        for root in self.roots:
            yield from root.walk()

    @property
    def element_count(self) -> int:
        return sum(1 for _ in self.walk())

    def first(self, element_id: int) -> Element | None:
        for item in self.walk():
            if item.element_id == element_id:
                return item
        return None


class _Parser:
    def __init__(self, data: bytes):
        self.data = data
        self.issues: list[dict] = []
        self.truncated = False
        self.truncation_offset: int | None = None

    def issue(self, offset: int, code: str, message: str, recoverable: bool = False) -> None:
        self.issues.append({
            "offset": offset,
            "code": code,
            "message": message,
            "recoverable": recoverable,
        })

    def _mark_truncated(self, offset: int, message: str) -> None:
        self.truncated = True
        self.truncation_offset = offset if self.truncation_offset is None else self.truncation_offset
        self.issue(offset, "truncated", message, recoverable=True)

    def parse(self) -> ParseResult:
        if not self.data:
            raise InputParseError("empty input")
        header = parse_element_header(self.data, 0)
        if header.element_id != EBML_ID:
            raise InputParseError("missing EBML header magic 0x1A45DFA3")
        if header.size is None:
            raise InputParseError("EBML header cannot have unknown size")

        roots: list[Element] = []
        offset = 0
        while offset < len(self.data):
            try:
                node = self._parse_element(offset, 0, 1, len(self.data), False)
            except InputParseError as exc:
                if roots and self.truncated:
                    self.issue(offset, "tail_error", str(exc), recoverable=True)
                    break
                raise
            if node is None:
                break
            roots.append(node)
            offset = node.end_offset
            if offset == node.offset:
                raise InputParseError(f"parser made no progress at offset {offset}")

        document = Element(
            element_id=0,
            name="Document",
            type=MASTER,
            offset=0,
            data_offset=0,
            size=len(self.data),
            end_offset=len(self.data),
            unknown_size=False,
            depth=0,
            header_length=0,
            children=roots,
        )
        for root in roots:
            root.parent = document
        result = ParseResult(
            data=self.data,
            roots=roots,
            issues=self.issues,
            truncated=self.truncated,
            truncation_offset=self.truncation_offset,
            document=document,
        )
        self._validate_header(result)
        return result

    def _validate_header(self, result: ParseResult) -> None:
        ebml = result.first(EBML_ID)
        if ebml is None:
            raise InputParseError("EBML header is missing")
        doc_type = ebml.child(0x4282)
        if doc_type is None or doc_type.value not in ("matroska", "webm"):
            raise InputParseError("EBML DocType is not matroska/webm")

    def _peek_id(self, offset: int) -> int | None:
        try:
            header = parse_element_header(self.data, offset)
            return header.element_id
        except InputParseError:
            return None

    def _parse_element(
        self,
        offset: int,
        parent_id: int,
        depth: int,
        outer_end: int,
        outer_unknown: bool,
    ) -> Element | None:
        header = parse_element_header(self.data, offset)
        spec = get_spec(header.element_id)
        data_offset = offset + header.header_length
        declared_end = None if header.size is None else data_offset + header.size

        if header.size is None and not is_master(header.element_id):
            raise InputParseError(
                f"unknown length used by non-master {spec.name} at offset {offset}"
            )
        if header.size is not None and declared_end > len(self.data):
            if outer_unknown:
                self._mark_truncated(
                    offset, f"{spec.name} payload extends past end of file"
                )
                return None
            raise InputParseError(
                f"{spec.name} at offset {offset} ends past file ({declared_end} > {len(self.data)})"
            )
        if header.size is not None and declared_end > outer_end:
            raise InputParseError(
                f"{spec.name} at offset {offset} exceeds parent boundary {outer_end}"
            )

        node = Element(
            element_id=header.element_id,
            name=spec.name,
            type=spec.type,
            offset=offset,
            data_offset=data_offset,
            size=header.size,
            end_offset=declared_end if declared_end is not None else data_offset,
            unknown_size=header.size is None,
            depth=depth,
            header_length=header.header_length,
        )

        if spec.type != MASTER:
            node.raw = self.data[data_offset : node.end_offset]
            return node

        child_boundary = declared_end if declared_end is not None else outer_end
        cursor = data_offset
        stop_reason = "declared_size" if declared_end is not None else "eof"
        while cursor < child_boundary:
            try:
                peek = parse_element_header(self.data, cursor)
            except InputParseError as exc:
                if declared_end is not None:
                    raise InputParseError(f"invalid child at offset {cursor}: {exc}") from exc
                self._mark_truncated(cursor, f"invalid child VINT: {exc}")
                stop_reason = "truncated"
                break

            if declared_end is None and not can_contain(header.element_id, peek.element_id):
                stop_reason = f"next_sibling:{element_name(peek.element_id)}"
                break
            if declared_end is not None and not can_contain(header.element_id, peek.element_id):
                raise InputParseError(
                    f"{element_name(peek.element_id)} at offset {cursor} "
                    f"is not a child of {spec.name}"
                )

            child = self._parse_element(
                cursor,
                header.element_id,
                depth + 1,
                child_boundary,
                declared_end is None,
            )
            if child is None:
                stop_reason = "truncated"
                break
            child.parent = node
            node.children.append(child)
            cursor = child.end_offset

        if declared_end is not None:
            node.end_offset = declared_end
            if cursor != declared_end:
                self.issue(
                    cursor,
                    "child_boundary_mismatch",
                    f"{spec.name} children end at {cursor}, declared end is {declared_end}",
                )
        else:
            node.end_offset = cursor
            node.boundary_reason = stop_reason
            if stop_reason == "truncated":
                node.truncated = True
        return node


def parse_ebml(data: bytes) -> ParseResult:
    return _Parser(data).parse()
