"""Sequential EBML parser: headers, nesting and unknown-size containers."""

from __future__ import annotations

from ._byte_reader import ByteReader
from ._ebml_types import Element, ElementKind, Header, ReaderError, ReaderOptions
from .ebml_schema import (
    UNKNOWN_SIZE_ALLOWED,
    SchemaError,
    Strictness,
    decode_vint,
    is_master,
    is_unknown_size,
    name_for,
    spec_for,
    vint_width,
)

MAX_ID_WIDTH = 4
class EbmlReader:
    """Parses an EBML stream into an :class:`Element` tree."""

    def __init__(self, reader: ByteReader, options: ReaderOptions | None = None) -> None:
        self.reader = reader
        self.options = options or ReaderOptions()
        self.element_count = 0
        self.unknown_length_count = 0
        self.warnings: list[dict[str, object]] = []

    # ── public API ─────────────────────────────────────────────────────
    def parse(self) -> Element:
        # The synthetic Document root is a container we invent, not a parsed
        # element, so it must not inflate element_count.
        root = Element(
            element_id=b"",
            name="Document",
            offset=0,
            id_width=0,
            size_width=0,
            data_offset=0,
            declared_size=self.reader.size,
            resolved_size=self.reader.size,
            unknown_size=False,
            depth=-1,
            kind=ElementKind.MASTER,
            end_offset=self.reader.size,
        )
        self._parse_children(root, expected_end=self.reader.size, field_name="Document")
        return root

    def read_header_at(self, offset: int) -> Element:
        """Parse exactly one element header at ``offset`` without descending."""
        self.reader.seek(offset)
        return self._read_element_header(depth=0)

    # ── internals ──────────────────────────────────────────────────────
    def _parse_children(self, parent: Element, *, expected_end: int, field_name: str) -> None:
        """Fill ``parent.children`` until ``expected_end`` (or a closing rule)."""
        while True:
            pos = self.reader.position
            if pos >= expected_end:
                if pos > expected_end:
                    raise ReaderError(
                        "ebml_overrun",
                        f"{field_name} ended at {expected_end}, reader at {pos}",
                        pos,
                    )
                return
            if pos >= self.reader.size:
                if expected_end > self.reader.size:
                    raise ReaderError(
                        "ebml_truncated",
                        f"{field_name} declared end {expected_end} past EOF {self.reader.size}",
                        pos,
                    )
                return
            # A child must exist; 1 byte is the minimum for an ID.
            header = self._read_element_header(depth=parent.depth + 1)
            if header.spec and not header.spec.accepts_parent(parent.name):
                if self.options.strictness is Strictness.STRICT:
                    raise ReaderError(
                        "ebml_parent_invalid",
                        f"{header.name} is not allowed inside {parent.name}",
                        header.offset,
                    )
                self._warn(
                    "parent_invalid",
                    f"{header.name} under {parent.name}; accepted in lenient mode",
                    header.offset,
                )
            if header.offset + header.id_width + header.size_width + max(header.declared_size, 0) > expected_end:
                if self.options.strictness is Strictness.STRICT:
                    raise ReaderError(
                        "ebml_truncated",
                        f"{header.name} at {header.offset} overruns {field_name} end {expected_end}",
                        header.offset,
                    )
                self._warn(
                    "child_overrun",
                    f"{header.name} overruns {field_name}; clamped in lenient mode",
                    header.offset,
                )
            self._materialize(parent, header, expected_end=expected_end)

    def _materialize(self, parent: Element, header: "Header", *, expected_end: int) -> None:
        self.element_count += 1
        if self.element_count > self.options.max_elements:
            raise ReaderError(
                "ebml_element_limit",
                f"more than {self.options.max_elements} elements",
                header.offset,
            )
        if header.depth > self.options.max_depth:
            raise ReaderError(
                "ebml_depth_limit",
                f"nesting deeper than {self.options.max_depth}",
                header.offset,
            )
        if header.unknown_size:
            self.unknown_length_count += 1
            if self.options.strictness is Strictness.STRICT and header.element_id not in UNKNOWN_SIZE_ALLOWED:
                raise ReaderError(
                    "ebml_unknown_size_illegal",
                    f"{header.name} may not use an unknown size",
                    header.offset,
                )
        element = Element(
            element_id=header.element_id,
            name=header.name,
            offset=header.offset,
            id_width=header.id_width,
            size_width=header.size_width,
            data_offset=header.data_offset,
            declared_size=header.declared_size,
            resolved_size=header.declared_size,
            unknown_size=header.unknown_size,
            depth=header.depth,
            kind=header.kind,
            spec=header.spec,
        )
        parent.children.append(element)

        if header.kind is ElementKind.LEAF:
            size = header.declared_size
            if self.options.retains(header.element_id, size):
                element.data = self.reader.read(size)
            else:
                self.reader.skip(size)
            element.end_offset = self.reader.position
            return

        # Master element.
        if not header.unknown_size:
            child_end = header.offset + header.id_width + header.size_width + header.declared_size
            if child_end > expected_end:
                raise ReaderError(
                    "ebml_truncated",
                    f"{header.name} declared size ends past parent boundary",
                    header.offset,
                )
            self._parse_children(element, expected_end=child_end, field_name=header.name)
            element.end_offset = self.reader.position
            element.resolved_size = element.end_offset - header.data_offset
            return

        # Unknown-size container: consume children until one belongs to an
        # ancestor level, or until the parent/EOF boundary.
        self._parse_unknown_children(element, parent=parent, boundary=expected_end)
        element.end_offset = self.reader.position
        element.resolved_size = element.end_offset - header.data_offset

    def _parse_unknown_children(self, element: Element, *, parent: Element, boundary: int) -> None:
        while True:
            pos = self.reader.position
            if pos >= boundary or pos >= self.reader.size:
                return
            peek_id, _ = self._peek_id(pos)
            spec = spec_for(peek_id)
            if spec is not None and not spec.accepts_parent(element.name):
                # Next element belongs above this container. Close here; the
                # caller's loop re-reads it in the correct parent.
                return
            if spec is None:
                # Unknown element: cannot prove ownership, keep it here.
                pass
            header = self._read_element_header(depth=element.depth + 1)
            if header.offset + header.id_width + header.size_width > boundary:
                raise ReaderError(
                    "ebml_truncated",
                    f"{header.name} header overruns parent boundary",
                    header.offset,
                )
            self._materialize(element, header, expected_end=boundary)

    def _read_element_header(self, *, depth: int) -> "Header":
        offset = self.reader.position
        first = self.reader.read(1)
        try:
            id_width = vint_width(first[0])
        except SchemaError as exc:
            raise ReaderError(exc.code, str(exc), offset) from exc
        if id_width > MAX_ID_WIDTH:
            raise ReaderError(
                "ebml_id_too_wide",
                f"element ID width {id_width} exceeds {MAX_ID_WIDTH} octets",
                offset,
            )
        id_bytes = first + self.reader.read(id_width - 1)
        element_id = id_bytes
        spec = spec_for(element_id)

        size_first = self.reader.read(1)
        try:
            size_width = vint_width(size_first[0])
        except SchemaError as exc:
            raise ReaderError(exc.code, str(exc), offset) from exc
        raw_size_bytes = size_first + self.reader.read(size_width - 1)
        try:
            raw_size, _ = decode_vint(raw_size_bytes, keep_marker=False, offset=offset)
        except SchemaError as exc:
            raise ReaderError(exc.code, str(exc), offset) from exc
        unknown = is_unknown_size(raw_size, size_width)

        if spec is None:
            kind = ElementKind.MASTER if is_master(element_id) else ElementKind.UNKNOWN
        elif spec.type == "master":
            kind = ElementKind.MASTER
        else:
            kind = ElementKind.LEAF

        if kind is ElementKind.UNKNOWN:
            # Unknown IDs are treated as opaque leaves: we cannot guess their
            # nesting semantics, which is exactly how a strict parser must fail.
            if self.options.strictness is Strictness.STRICT:
                raise ReaderError(
                    "ebml_unknown_element",
                    f"element ID {element_id.hex()} is not in the schema",
                    offset,
                )
            self._warn("unknown_element", f"unknown element {element_id.hex()}", offset)
            kind = ElementKind.LEAF

        if unknown and kind is not ElementKind.MASTER:
            raise ReaderError(
                "ebml_unknown_size_illegal",
                f"{name_for(element_id)} is not a master element and cannot be unknown-sized",
                offset,
            )

        data_offset = self.reader.position
        return Header(
            element_id=element_id,
            name=name_for(element_id),
            offset=offset,
            id_width=id_width,
            size_width=size_width,
            data_offset=data_offset,
            declared_size=-1 if unknown else raw_size,
            unknown_size=unknown,
            kind=kind,
            spec=spec,
            depth=depth,
        )

    def _peek_id(self, pos: int) -> tuple[bytes, int]:
        self.reader.seek(pos)
        peek = self.reader.peek(MAX_ID_WIDTH)
        if not peek:
            return b"", 0
        try:
            width = vint_width(peek[0])
        except SchemaError as exc:
            raise ReaderError(exc.code, str(exc), pos) from exc
        if width > MAX_ID_WIDTH:
            raise ReaderError("ebml_id_too_wide", "element ID wider than 4 octets", pos)
        if len(peek) < width:
            raise ReaderError("ebml_truncated", "truncated element ID at EOF", pos)
        return peek[:width], width

    def _warn(self, code: str, message: str, offset: int | None) -> None:
        self.warnings.append({"code": code, "message": message, "offset": offset})


