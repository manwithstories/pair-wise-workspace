"""sidx (segment index) reading and writing, version 0 and 1."""
from __future__ import annotations

import struct
from dataclasses import dataclass, field

from .boxio import BoxError

__all__ = ["Sidx", "SidxReference", "parse_sidx", "build_sidx", "reference_spans"]


@dataclass
class SidxReference:
    reference_type: int          # 0 = media, 1 = nested sidx
    referenced_size: int         # 31 bits
    subsegment_duration: int
    starts_with_sap: bool = True
    sap_type: int = 1
    sap_delta_time: int = 0


@dataclass
class Sidx:
    version: int
    reference_id: int
    timescale: int
    earliest_presentation_time: int
    first_offset: int
    references: list = field(default_factory=list)


def parse_sidx(reader, box):
    version, _flags, off = reader.fullbox(box)
    if version not in (0, 1):
        raise BoxError("unsupported sidx version %d at %d" % (version, box.start))
    reference_id, timescale = reader.unpack(">II", off)
    off += 8
    if version == 1:
        ept, first_offset = reader.unpack(">QQ", off)
        off += 16
    else:
        ept, first_offset = reader.unpack(">II", off)
        off += 8
    _reserved, count = reader.unpack(">HH", off)
    off += 4
    refs = []
    for _ in range(count):
        size_word, dur, sap_word = reader.unpack(">III", off)
        off += 12
        refs.append(SidxReference(
            reference_type=(size_word >> 31) & 1,
            referenced_size=size_word & 0x7FFFFFFF,
            subsegment_duration=dur,
            starts_with_sap=bool((sap_word >> 31) & 1),
            sap_type=(sap_word >> 28) & 0x7,
            sap_delta_time=sap_word & 0x0FFFFFFF,
        ))
    return Sidx(version, reference_id, timescale, ept, first_offset, refs)


def build_sidx(sidx):
    """Serialise; bump to version 1 when 32-bit fields overflow."""
    for ref in sidx.references:
        if ref.referenced_size > 0x7FFFFFFF:
            raise BoxError(
                "referenced_size %d exceeds 31 bits" % ref.referenced_size)
    version = sidx.version
    if (sidx.earliest_presentation_time > 0xFFFFFFFF
            or sidx.first_offset > 0xFFFFFFFF):
        version = 1
    out = bytearray()
    out += struct.pack(">B", version) + b"\x00\x00\x00"
    out += struct.pack(">II", sidx.reference_id, sidx.timescale)
    if version == 1:
        out += struct.pack(">QQ", sidx.earliest_presentation_time,
                           sidx.first_offset)
    else:
        out += struct.pack(">II", sidx.earliest_presentation_time,
                           sidx.first_offset)
    out += struct.pack(">HH", 0, len(sidx.references))
    for ref in sidx.references:
        size_word = ((ref.reference_type & 1) << 31) | ref.referenced_size
        sap_word = (((1 if ref.starts_with_sap else 0) << 31)
                    | ((ref.sap_type & 0x7) << 28)
                    | (ref.sap_delta_time & 0x0FFFFFFF))
        out += struct.pack(">III", size_word, ref.subsegment_duration, sap_word)
    return struct.pack(">I4s", 8 + len(out), b"sidx") + bytes(out)


def reference_spans(sidx, sidx_end):
    """Absolute (offset, size, reference) triples in document order."""
    spans = []
    pos = sidx_end + sidx.first_offset
    for ref in sidx.references:
        spans.append((pos, ref.referenced_size, ref))
        pos += ref.referenced_size
    return spans

