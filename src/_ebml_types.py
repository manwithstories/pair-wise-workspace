"""EBML element, header and error records shared across layers."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Iterator

from .ebml_schema import ElementSpec, SchemaError, Strictness


class ReaderError(SchemaError):
    """Structural EBML failure: truncation, bad ID, illegal unknown size."""


class ElementKind(Enum):
    MASTER = "master"
    LEAF = "leaf"
    UNKNOWN = "unknown"


@dataclass
class Element:
    """One parsed EBML element with absolute offsets into the source."""

    element_id: bytes
    name: str
    offset: int
    id_width: int
    size_width: int
    data_offset: int
    declared_size: int
    resolved_size: int
    unknown_size: bool
    depth: int
    kind: ElementKind
    spec: ElementSpec | None = None
    data: bytes | None = None
    children: list["Element"] = field(default_factory=list)
    end_offset: int = 0

    @property
    def is_master(self) -> bool:
        return self.kind is ElementKind.MASTER

    def child(self, element_id: bytes) -> "Element | None":
        for item in self.children:
            if item.element_id == element_id:
                return item
        return None

    def children_named(self, element_id: bytes) -> list["Element"]:
        return [item for item in self.children if item.element_id == element_id]

    def walk(self) -> Iterator["Element"]:
        yield self
        for item in self.children:
            yield from item.walk()

    def as_summary(self) -> dict[str, object]:
        row: dict[str, object] = {
            "name": self.name,
            "id": self.element_id.hex(),
            "offset": self.offset,
            "header_size": self.id_width + self.size_width,
            "size": self.resolved_size,
            "unknown_size": self.unknown_size,
            "depth": self.depth,
        }
        if self.kind is ElementKind.LEAF:
            row["data_offset"] = self.data_offset
        else:
            row["child_count"] = len(self.children)
        return row


@dataclass
class Header:
    element_id: bytes
    name: str
    offset: int
    id_width: int
    size_width: int
    data_offset: int
    declared_size: int
    unknown_size: bool
    kind: ElementKind
    spec: ElementSpec | None
    depth: int


@dataclass
class ReaderOptions:
    strictness: Strictness = Strictness.STRICT
    max_elements: int = 100_000
    max_depth: int = 32
    #: Leaf payloads larger than this are left on disk (data=None) and only
    #: described by data_offset/declared_size.
    max_payload: int = 8 << 20
    #: Element IDs whose payload should be retained regardless of size.
    keep_payload_ids: frozenset[bytes] = frozenset()

    def retains(self, element_id: bytes, size: int) -> bool:
        if element_id in self.keep_payload_ids:
            return True
        return size <= self.max_payload
