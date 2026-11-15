"""Streaming EBML cursor.

Parses element headers on demand and yields payload ranges; payloads are
never loaded unless explicitly requested, so memory stays flat regardless
of file size.  Unknown-size master elements are delimited by the next
segment-level sibling or by EOF.
"""
from __future__ import annotations

import os

from .schema import (
    ELEMENTS, MASTER, ID_CLUSTER, ID_SEGMENT, SEGMENT_LEVEL_IDS,
)
from .vint import read_vint, VintTruncated, VintInvalid

HEADER_WINDOW = 16  # worst case: 8-byte ID + 8-byte size


class EbmlError(Exception):
    """Structural EBML problem (maps to CLI exit code 2)."""


class TruncatedError(EbmlError):
    """The file ends in the middle of an element."""


class NotMatroskaError(EbmlError):
    """The input is not a Matroska/WebM file."""


class Element:
    """One parsed element header (plus children for master elements)."""

    __slots__ = (
        "id", "name", "type", "offset", "size", "header_size",
        "unknown_size", "depth", "data_offset", "end", "children",
        "truncated",
    )

    def __init__(self, eid, offset, size, header_size, unknown_size, depth):
        self.id = eid
        self.name, self.type = ELEMENTS.get(eid, ("Unknown", "binary"))
        self.offset = offset
        self.size = size
        self.header_size = header_size
        self.unknown_size = unknown_size
        self.depth = depth
        self.data_offset = offset + header_size
        self.end = -1 if unknown_size else self.data_offset + size
        self.children: list[Element] = []
        self.truncated = False

    def __repr__(self):  # pragma: no cover - debugging aid
        return f"<Element {self.name} @{self.offset} size={self.size}>"


class EbmlReader:
    """Header-level cursor over an EBML file."""

    def __init__(self, path, tolerant: bool = False):
        self.path = os.fspath(path)
        self.file_size = os.path.getsize(self.path)
        self.tolerant = tolerant
        self.truncations: list[int] = []
        self._fh = open(self.path, "rb")

    def close(self):
        self._fh.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    def read_at(self, offset: int, size: int) -> bytes:
        self._fh.seek(offset)
        return self._fh.read(size)

    def read_header(self, offset: int, depth: int = 0) -> Element:
        if offset >= self.file_size:
            raise TruncatedError(f"element header beyond EOF at {offset}")
        head = self.read_at(offset, HEADER_WINDOW)
        try:
            eid, id_len, _ = read_vint(head, 0, strip_marker=False)
            size, size_len, unknown = read_vint(head, id_len, strip_marker=True)
        except VintTruncated as exc:
            raise TruncatedError(f"truncated header at {offset}: {exc}") from exc
        except VintInvalid as exc:
            raise EbmlError(f"invalid element header at {offset}: {exc}") from exc
        if id_len > 4:
            raise EbmlError(f"element ID at {offset} exceeds 4 bytes")
        return Element(eid, offset, size, id_len + size_len, unknown, depth)

    def read_payload(self, el: Element) -> bytes:
        data = self.read_at(el.data_offset, el.size)
        if len(data) != el.size:
            raise TruncatedError(
                f"element {el.name} at {el.offset}: payload truncated"
            )
        return data

    def parse(self) -> list[Element]:
        """Parse the whole file into a list of top-level elements."""
        out: list[Element] = []
        self._parse_level(0, self.file_size, 0, None, out)
        return out

    def _parse_level(self, start, end, depth, stop_ids, out) -> int:
        pos = start
        while pos < end:
            try:
                el = self.read_header(pos, depth)
            except TruncatedError:
                if not self.tolerant:
                    raise
                self.truncations.append(pos)
                return end
            if stop_ids and el.id in stop_ids:
                break
            if el.unknown_size:
                if el.type != MASTER:
                    raise EbmlError(
                        f"non-master element 0x{el.id:X} with unknown size"
                    )
                if el.id == ID_CLUSTER:
                    child_stop = SEGMENT_LEVEL_IDS
                elif el.id == ID_SEGMENT:
                    child_stop = frozenset((ID_SEGMENT,))
                else:
                    child_stop = None
                child_end = self._parse_level(
                    el.data_offset, end, depth + 1, child_stop, el.children
                )
                el.end = child_end
                el.size = child_end - el.data_offset
            else:
                el.end = el.data_offset + el.size
                if el.end > self.file_size:
                    if not self.tolerant:
                        raise TruncatedError(
                            f"element {el.name} at {el.offset} overruns EOF"
                        )
                    self.truncations.append(el.offset)
                    el.truncated = True
                    el.end = self.file_size
                    out.append(el)
                    return end
                if el.type == MASTER:
                    self._parse_level(
                        el.data_offset, el.end, depth + 1, None, el.children
                    )
            out.append(el)
            pos = el.end
        return pos
