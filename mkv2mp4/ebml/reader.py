"""Streaming EBML element reader.

Elements are yielded in document order as a flat stream with depth
annotations; payload data is never buffered unless explicitly
requested, so arbitrarily large files parse in constant memory.

Unknown-length master elements (Segment/Cluster left behind by
interrupted recordings) are terminated by the parent's boundary or,
for level-1 masters, by the appearance of the next top-level element
(stack-based backtracking).  Any read past the parent boundary fails
immediately with an offset-carrying EbmlError.
"""

from dataclasses import dataclass

from . import schema
from .vint import read_element_id, read_element_size
from ..errors import EbmlError


@dataclass
class Element:
    id: int
    name: str
    type: str
    offset: int       # absolute offset of the element ID in the file
    depth: int        # 1 for top-level elements (EBML, Segment)
    size: object      # int, or None for unknown length
    header_size: int

    @property
    def is_master(self):
        return self.type == "master"

    @property
    def data_offset(self):
        return self.offset + self.header_size

    @property
    def data_end(self):
        return None if self.size is None else self.data_offset + self.size


class ElementReader:
    """Cursor-based traversal over a seekable binary file object."""

    def __init__(self, fh, size):
        self.fh = fh
        self.size = size

    def read_header(self, pos, depth, parent_end):
        self.fh.seek(pos)
        element_id, id_len = read_element_id(self.fh, pos)
        size, size_len = read_element_size(self.fh, pos + id_len)
        spec = schema.spec_for(element_id)
        if size is None and spec.type != "master":
            raise EbmlError(
                "unknown length on non-master element %s" % spec.name, pos)
        el = Element(element_id, spec.name, spec.type, pos, depth,
                     size, id_len + size_len)
        if el.data_end is not None and el.data_end > parent_end:
            raise EbmlError(
                "element %s overruns parent boundary" % spec.name, pos)
        return el

    def walk(self):
        """Yield every element in document order (streaming)."""
        return self._walk(0, self.size, 1)

    def _walk(self, pos, end, depth, stop_at_top=False):
        while pos < end:
            el = self.read_header(pos, depth, end)
            if stop_at_top and el.id in schema.TOP_LEVEL_IDS:
                # Unknown-length level-1 master ends right before the
                # next top-level element; push it back for the caller.
                return pos
            yield el
            if el.is_master:
                child_end = el.data_end if el.size is not None else end
                stop = el.size is None and el.id != schema.SEGMENT_ID
                pos = yield from self._walk(
                    el.data_offset, child_end, depth + 1, stop)
            else:
                pos = el.data_end
        return end

    def read_data(self, el):
        """Read one element's payload (only call for small elements)."""
        if el.size is None:
            raise EbmlError(
                "cannot read payload of unknown-length element", el.offset)
        self.fh.seek(el.data_offset)
        data = self.fh.read(el.size)
        if len(data) != el.size:
            raise EbmlError("truncated element payload", el.offset)
        return data
