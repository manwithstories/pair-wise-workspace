"""Streaming EBML reader: IDs, variable-length sizes, and unknown-size elements.

The reader is sequential: it walks a byte source front to back, reading only
element headers plus the payloads the caller asks to keep. Master element
boundaries are tracked so that an unknown-size Segment or Cluster is closed at
its parent's end or at EOF, and so that a known-size container that ends in the
middle of a child is reported as truncation instead of silently accepted.

The implementation is split for readability: :mod:`src._byte_reader` owns the
byte source, :mod:`src._ebml_types` the records, :mod:`src._ebml_parser` the
state machine. This module is the stable public surface.
"""

from __future__ import annotations

from typing import BinaryIO, Iterable

from ._byte_reader import ByteReader
from ._ebml_parser import MAX_ID_WIDTH, EbmlReader, ReaderOptions
from ._ebml_types import Element, ElementKind, Header, ReaderError
from .ebml_schema import name_for

__all__ = [
    "ByteReader",
    "EbmlReader",
    "Element",
    "ElementKind",
    "Header",
    "MAX_ID_WIDTH",
    "ReaderError",
    "ReaderOptions",
    "open_source",
    "parse_bytes",
    "iter_elements",
    "require_master",
]

# ── source helpers ────────────────────────────────────────────────────────


def open_source(source: str | Path | bytes | BinaryIO) -> tuple[BinaryIO, int, bool]:
    """Return ``(fileobj, size, should_close)`` for paths, bytes, or handles."""
    if isinstance(source, (str, Path)):
        path = Path(source)
        fp = path.open("rb")
        return fp, path.stat().st_size, True
    if isinstance(source, (bytes, bytearray, memoryview)):
        import io

        data = bytes(source)
        return io.BytesIO(data), len(data), True
    fp = source
    cur = fp.tell()
    fp.seek(0, 2)
    size = fp.tell()
    fp.seek(cur)
    return fp, size, False


def parse_bytes(data: bytes, options: ReaderOptions | None = None) -> tuple[Element, EbmlReader]:
    """Convenience parser over an in-memory document."""
    reader = EbmlReader(ByteReader(open_bytes(data)), options)
    return reader.parse(), reader


def open_bytes(data: bytes):
    import io

    return io.BytesIO(data)


def iter_elements(root: Element) -> Iterable[Element]:
    """Depth-first iteration in document order."""
    yield from root.walk()


def require_master(parent: Element, element_id: bytes, *, required: bool = True) -> Element | None:
    found = parent.child(element_id)
    if found is None and required:
        raise ReaderError(
            "ebml_missing_element",
            f"{name_for(element_id)} missing inside {parent.name}",
            parent.offset,
        )
    return found


__all__ = [
    "ByteReader",
    "EbmlReader",
    "Element",
    "ElementKind",
    "Header",
    "MAX_ID_WIDTH",
    "ReaderError",
    "ReaderOptions",
    "open_source",
    "parse_bytes",
    "iter_elements",
    "require_master",
]
