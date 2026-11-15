"""Box tree construction and path-based lookup."""
from __future__ import annotations

from dataclasses import dataclass, field

from .reader import BoxHeader, ByteCursor, StructureError

# Box types whose payload is a sequence of child boxes.
CONTAINER_TYPES = frozenset({
    "moov", "trak", "mdia", "minf", "stbl", "moof", "traf", "mvex",
    "edts", "dinf", "mfra", "skip", "ipro", "sinf", "schi", "tref",
})


@dataclass
class Box:
    """A node in the parsed box tree."""

    header: BoxHeader
    children: list["Box"] = field(default_factory=list)
    parent: "Box | None" = None

    @property
    def type(self) -> str:
        return self.header.type

    @property
    def offset(self) -> int:
        return self.header.offset

    @property
    def size(self) -> int:
        return self.header.size

    @property
    def end(self) -> int:
        return self.header.end

    @property
    def payload_offset(self) -> int:
        return self.header.payload_offset

    @property
    def path(self) -> str:
        parts = []
        node: Box | None = self
        while node is not None and node.type:
            parts.append(node.type)
            node = node.parent
        return "/".join(reversed(parts))

    def iter(self):
        """Depth-first iteration over self and all descendants."""
        yield self
        for child in self.children:
            yield from child.iter()

    def find(self, path: str) -> list["Box"]:
        """Find descendants matching a slash-separated type path."""
        parts = [p for p in path.split("/") if p]
        if not parts:
            return []
        results: list[Box] = []

        def walk(node: Box, depth: int) -> None:
            if depth == len(parts):
                results.append(node)
                return
            for child in node.children:
                if child.type == parts[depth]:
                    walk(child, depth + 1)

        if self.type == parts[0]:
            walk(self, 1)
        else:
            for child in self.children:
                if child.type == parts[0]:
                    walk(child, 1)
        return results

    def find_one(self, path: str) -> "Box | None":
        matches = self.find(path)
        return matches[0] if matches else None


def _parse_children(cursor: ByteCursor, parent: Box) -> None:
    offset = parent.payload_offset
    end = parent.end
    while offset < end:
        header = cursor.parse_header(offset, parent_end=end)
        child = Box(header=header, parent=parent)
        parent.children.append(child)
        if header.type in CONTAINER_TYPES:
            _parse_children(cursor, child)
        offset = header.end
    if offset != end:
        raise StructureError("child boxes do not align to parent end", parent.offset)


def build_tree(cursor: ByteCursor) -> Box:
    """Parse the whole file into a box tree rooted at a virtual root."""
    root_header = BoxHeader(type="", offset=0, size=cursor.size, header_size=0)
    root = Box(header=root_header)
    _parse_children(cursor, root)
    return root


def mdat_ranges(root: Box) -> list[tuple[int, int]]:
    """Return (payload_start, payload_end) for every top-level mdat."""
    return [
        (box.payload_offset, box.end)
        for box in root.children
        if box.type == "mdat"
    ]
