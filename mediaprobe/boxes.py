from __future__ import annotations

import os
import struct
from dataclasses import dataclass, field
from typing import BinaryIO, Iterable, Iterator

from .errors import ContainerError, EXIT_PARSE, MediaError


BOX_HEADER = 8
LARGE_HEADER = 16
UUID_HEADER = 24
MAX_NESTING = 32

# These boxes carry ordinary child boxes. Sample entries inside stsd are
# parsed by isobmff.py because their first payload fields are not a box list.
CONTAINER_TYPES = frozenset(
    {"moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "mvex", "moof", "traf"}
)


@dataclass
class Box:
    type: str
    offset: int
    size: int
    header_size: int
    path: str
    user_type: bytes | None = None
    children: list["Box"] = field(default_factory=list)

    @property
    def data_offset(self) -> int:
        return self.offset + self.header_size

    @property
    def data_size(self) -> int:
        return self.size - self.header_size

    @property
    def end(self) -> int:
        return self.offset + self.size

    def child(self, box_type: str) -> "Box | None":
        return next((item for item in self.children if item.type == box_type), None)

    def children_of(self, box_type: str) -> list["Box"]:
        return [item for item in self.children if item.type == box_type]


def _box_kind(header: bytes) -> str:
    return header[4:8].decode("latin-1", "replace")


def _read_exact(fh: BinaryIO, offset: int, count: int) -> bytes:
    fh.seek(offset)
    data = fh.read(count)
    if len(data) != count:
        raise MediaError(
            "FILE_TRUNCATED",
            f"expected {count} bytes at offset {offset}, got {len(data)}",
            offset=offset,
            exit_code=EXIT_PARSE,
        )
    return data


def _scan_boxes(
    fh: BinaryIO,
    start: int,
    end: int,
    parent_path: str,
    depth: int,
) -> list[Box]:
    if depth > MAX_NESTING:
        raise ContainerError(
            "BOX_MALFORMED",
            "box nesting exceeds safety limit",
            offset=start,
        )
    boxes: list[Box] = []
    position = start
    sibling_index = 0
    while position < end:
        remaining = end - position
        if remaining < BOX_HEADER:
            raise MediaError(
                "FILE_TRUNCATED",
                f"{remaining} trailing bytes cannot form a complete box header",
                offset=position,
            )
        header = _read_exact(fh, position, BOX_HEADER)
        size32 = struct.unpack_from(">I", header, 0)[0]
        box_type = _box_kind(header)
        header_size = BOX_HEADER
        if size32 == 1:
            if remaining < LARGE_HEADER:
                raise MediaError(
                    "FILE_TRUNCATED",
                    "64-bit box header is truncated",
                    offset=position,
                )
            extra = _read_exact(fh, position + BOX_HEADER, 8)
            size = struct.unpack(">Q", extra)[0]
            header_size = LARGE_HEADER
        elif size32 == 0:
            size = end - position
        else:
            size = size32

        if box_type == "uuid":
            header_size += 16
            if size < header_size:
                raise ContainerError(
                    "BOX_MALFORMED",
                    "uuid box is shorter than its header",
                    offset=position,
                )

        if size < header_size:
            raise ContainerError(
                "BOX_MALFORMED",
                f"declared size {size} is smaller than header {header_size}",
                offset=position,
            )
        if position + size > end:
            raise MediaError(
                "FILE_TRUNCATED",
                f"box {box_type} extends past its container end",
                offset=position,
            )

        segment = box_type
        if parent_path:
            segment = f"{box_type}[{sibling_index}]"
            box_path = f"{parent_path}/{segment}"
        else:
            box_path = segment
        user_type = None
        if box_type == "uuid":
            user_type = _read_exact(fh, position + header_size - 16, 16)
        node = Box(box_type, position, size, header_size, box_path, user_type)
        if box_type in CONTAINER_TYPES:
            node.children = _scan_boxes(fh, node.data_offset, node.end, box_path, depth + 1)
        boxes.append(node)
        position += size
        sibling_index += 1

    if position != end:
        raise ContainerError(
            "BOX_MALFORMED",
            f"box scan ended at {position}, expected {end}",
            offset=position,
        )
    return boxes


def parse_boxes(source: str | os.PathLike[str] | BinaryIO, *, start: int = 0, end: int | None = None) -> list[Box]:
    """Return the box tree without reading media payloads.

    ``start`` and ``end`` permit callers to parse a bounded slice. A path input
    is kept open only for the duration of this call; a file object remains owned
    by the caller.
    """

    owns_file = not hasattr(source, "read")
    fh: BinaryIO
    if owns_file:
        try:
            fh = open(source, "rb")
        except PermissionError as exc:
            raise MediaError("PERMISSION_DENIED", str(exc), path=str(source), exit_code=2) from exc
        except OSError as exc:
            raise MediaError("IO_ERROR", str(exc), path=str(source), exit_code=2) from exc
    else:
        fh = source  # type: ignore[assignment]
    try:
        fh.seek(0, os.SEEK_END)
        size = fh.tell()
        upper = size if end is None else min(end, size)
        lower = start
        if lower < 0 or lower > upper:
            raise ContainerError("BOX_MALFORMED", "invalid box scan range", offset=lower)
        return _scan_boxes(fh, lower, upper, "", 0)
    finally:
        if owns_file:
            fh.close()


def read_box_data(fh: BinaryIO, box: Box, *, limit: int | None = None) -> bytes:
    count = box.data_size
    if limit is not None and count > limit:
        raise ContainerError(
            "BOX_TOO_LARGE",
            f"box {box.path} payload exceeds bounded read limit",
            offset=box.offset,
        )
    return _read_exact(fh, box.data_offset, count)


def walk(boxes: Iterable[Box]) -> Iterator[Box]:
    for item in boxes:
        yield item
        yield from walk(item.children)


def find_box(boxes: Iterable[Box], box_type: str) -> Box | None:
    return next((item for item in walk(boxes) if item.type == box_type), None)


def find_boxes(boxes: Iterable[Box], box_type: str) -> list[Box]:
    return [item for item in walk(boxes) if item.type == box_type]


def parent_chain(box: Box) -> list[str]:
    parts = box.path.split("/")
    return ["/".join(parts[:index]) for index in range(1, len(parts))]
