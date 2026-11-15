"""Segment index (sidx) parsing with hierarchical reference expansion."""
from __future__ import annotations

from dataclasses import dataclass, field

from .boxes import Box
from .reader import ByteCursor, StructureError

MAX_SIDX_DEPTH = 8


@dataclass
class SidxReference:
    reference_type: int          # 0 = media segment, 1 = nested sidx
    referenced_size: int
    subsegment_duration: int
    starts_with_sap: bool
    sap_type: int
    sap_delta_time: int
    byte_start: int = 0          # absolute file offset, filled during expansion

    @property
    def byte_end(self) -> int:
        return self.byte_start + self.referenced_size


@dataclass
class SidxIndex:
    version: int
    reference_id: int
    timescale: int
    earliest_presentation_time: int
    first_offset: int
    references: list[SidxReference] = field(default_factory=list)
    box_offset: int = 0
    box_end: int = 0


def parse_sidx(cursor: ByteCursor, box: Box) -> SidxIndex:
    """Parse one sidx box (version 0 or 1)."""
    pos = box.payload_offset
    if pos + 4 > box.end:
        raise StructureError("truncated sidx fullbox header", box.offset)
    version = cursor.u8(pos)
    if version not in (0, 1):
        raise StructureError(f"unsupported sidx version {version}", box.offset)
    pos += 4
    reference_id = cursor.u32(pos)
    timescale = cursor.u32(pos + 4)
    pos += 8
    if version == 0:
        earliest = cursor.u32(pos)
        first_offset = cursor.u32(pos + 4)
        pos += 8
    else:
        earliest = cursor.u64(pos)
        first_offset = cursor.u64(pos + 8)
        pos += 16
    pos += 2  # reserved
    reference_count = cursor.u16(pos)
    pos += 2
    references: list[SidxReference] = []
    for _ in range(reference_count):
        if pos + 12 > box.end:
            raise StructureError("sidx references overrun box", box.offset)
        raw = cursor.u32(pos)
        sap_word = cursor.u32(pos + 8)
        references.append(SidxReference(
            reference_type=(raw >> 31) & 0x1,
            referenced_size=raw & 0x7FFFFFFF,
            subsegment_duration=cursor.u32(pos + 4),
            starts_with_sap=bool(sap_word >> 31),
            sap_type=(sap_word >> 28) & 0x7,
            sap_delta_time=sap_word & 0x0FFFFFFF,
        ))
        pos += 12
    return SidxIndex(
        version=version, reference_id=reference_id, timescale=timescale,
        earliest_presentation_time=earliest, first_offset=first_offset,
        references=references, box_offset=box.offset, box_end=box.end,
    )


def expand_sidx(cursor: ByteCursor, sidx: SidxIndex,
                _depth: int = 0, _seen: frozenset[int] = frozenset()
                ) -> list[SidxReference]:
    """Flatten references; reference_type 1 recurses into nested sidx boxes.

    Returns leaf references with absolute byte ranges, in presentation order.
    """
    if _depth > MAX_SIDX_DEPTH:
        raise StructureError("sidx hierarchy too deep", sidx.box_offset)
    if sidx.box_offset in _seen:
        raise StructureError("cyclic sidx reference", sidx.box_offset)
    seen = _seen | {sidx.box_offset}
    leaves: list[SidxReference] = []
    cursor_pos = sidx.box_end + sidx.first_offset
    for ref in sidx.references:
        ref.byte_start = cursor_pos
        if ref.reference_type == 1:
            if ref.byte_start < 0 or ref.byte_end > cursor.size:
                raise StructureError("sidx reference outside file",
                                     sidx.box_offset)
            header = cursor.parse_header(ref.byte_start)
            if header.type != "sidx":
                raise StructureError("sidx reference does not point to sidx",
                                     ref.byte_start)
            nested = parse_sidx(cursor, Box(header=header))
            leaves.extend(expand_sidx(cursor, nested, _depth + 1, seen))
        else:
            leaves.append(ref)
        cursor_pos = ref.byte_end
    return leaves


def cumulative_durations(references: list[SidxReference]) -> list[int]:
    """Cumulative subsegment durations, one entry per reference."""
    totals: list[int] = []
    running = 0
    for ref in references:
        running += ref.subsegment_duration
        totals.append(running)
    return totals
