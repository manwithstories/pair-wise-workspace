"""Expansion of the six stbl sample tables into per-sample records."""
from __future__ import annotations

from dataclasses import dataclass

from .boxes import Box
from .reader import ByteCursor, StructureError


@dataclass
class Sample:
    index: int
    dts: int
    pts: int
    size: int
    offset: int
    chunk_index: int


def _fullbox(cursor: ByteCursor, box: Box) -> tuple[int, int, int]:
    """Return (version, flags, payload_offset_after_fullbox_header)."""
    if box.payload_offset + 4 > box.end:
        raise StructureError(f"truncated fullbox header in '{box.type}'", box.offset)
    version = cursor.u8(box.payload_offset)
    flags = cursor.u24(box.payload_offset + 1)
    return version, flags, box.payload_offset + 4


def parse_stts(cursor: ByteCursor, box: Box) -> list[tuple[int, int]]:
    """Decode time-to-sample: list of (sample_count, sample_delta)."""
    _, _, pos = _fullbox(cursor, box)
    count = cursor.u32(pos)
    pos += 4
    entries = []
    for _ in range(count):
        entries.append((cursor.u32(pos), cursor.u32(pos + 4)))
        pos += 8
    if pos > box.end:
        raise StructureError("stts entries overrun box", box.offset)
    return entries


def parse_ctts(cursor: ByteCursor, box: Box) -> list[int]:
    """Composition offsets expanded per sample; signed for version 1."""
    version, _, pos = _fullbox(cursor, box)
    count = cursor.u32(pos)
    pos += 4
    offsets: list[int] = []
    for _ in range(count):
        sample_count = cursor.u32(pos)
        raw = cursor.i32(pos + 4) if version == 1 else cursor.u32(pos + 4)
        offsets.extend([raw] * sample_count)
        pos += 8
    if pos > box.end:
        raise StructureError("ctts entries overrun box", box.offset)
    return offsets


def parse_stsc(cursor: ByteCursor, box: Box) -> list[tuple[int, int, int]]:
    """Sample-to-chunk entries: (first_chunk, samples_per_chunk, description)."""
    _, _, pos = _fullbox(cursor, box)
    count = cursor.u32(pos)
    pos += 4
    entries = []
    prev_first = 0
    for _ in range(count):
        first_chunk = cursor.u32(pos)
        per_chunk = cursor.u32(pos + 4)
        desc = cursor.u32(pos + 8)
        if first_chunk <= prev_first:
            raise StructureError("stsc first_chunk not increasing", box.offset)
        if per_chunk == 0:
            raise StructureError("stsc samples_per_chunk is zero", box.offset)
        entries.append((first_chunk, per_chunk, desc))
        prev_first = first_chunk
        pos += 12
    if pos > box.end:
        raise StructureError("stsc entries overrun box", box.offset)
    return entries


def parse_stsz(cursor: ByteCursor, box: Box) -> list[int]:
    """Sample sizes; constant when sample_size != 0, else per-sample table."""
    _, _, pos = _fullbox(cursor, box)
    sample_size = cursor.u32(pos)
    sample_count = cursor.u32(pos + 4)
    pos += 8
    if sample_size != 0:
        return [sample_size] * sample_count
    if pos + 4 * sample_count > box.end:
        raise StructureError("stsz entries overrun box", box.offset)
    return [cursor.u32(pos + 4 * i) for i in range(sample_count)]


def parse_chunk_offsets(cursor: ByteCursor, stbl: Box) -> list[int]:
    """Chunk offsets from stco (32-bit) and/or co64 (64-bit), in box order."""
    offsets: list[int] = []
    found = False
    for child in stbl.children:
        if child.type == "stco":
            _, _, pos = _fullbox(cursor, child)
            count = cursor.u32(pos)
            offsets.extend(cursor.u32(pos + 4 + 4 * i) for i in range(count))
            found = True
        elif child.type == "co64":
            _, _, pos = _fullbox(cursor, child)
            count = cursor.u32(pos)
            offsets.extend(cursor.u64(pos + 4 + 8 * i) for i in range(count))
            found = True
    if not found:
        raise StructureError("no stco/co64 chunk offset table", stbl.offset)
    return offsets


def expand_samples(cursor: ByteCursor, stbl: Box) -> list[Sample]:
    """Expand stts/ctts/stsc/stsz/stco into one record per sample."""
    stts_box = stbl.find_one("stts")
    stsz_box = stbl.find_one("stsz")
    stsc_box = stbl.find_one("stsc")
    if stts_box is None or stsz_box is None or stsc_box is None:
        raise StructureError("stbl missing stts/stsc/stsz", stbl.offset)
    stts = parse_stts(cursor, stts_box)
    sizes = parse_stsz(cursor, stsz_box)
    stsc = parse_stsc(cursor, stsc_box)
    chunk_offsets = parse_chunk_offsets(cursor, stbl)
    sample_count = len(sizes)
    ctts_box = stbl.find_one("ctts")
    ctts = parse_ctts(cursor, ctts_box) if ctts_box else [0] * sample_count
    if len(ctts) < sample_count:
        raise StructureError("ctts shorter than sample count", stbl.offset)
    chunk_count = len(chunk_offsets)
    per_chunk = [0] * chunk_count
    for idx, (first_chunk, samples_per, _desc) in enumerate(stsc):
        nxt = stsc[idx + 1][0] if idx + 1 < len(stsc) else chunk_count + 1
        if first_chunk > chunk_count:
            raise StructureError(
                f"stsc first_chunk {first_chunk} exceeds chunk count {chunk_count}",
                stsc_box.offset,
            )
        for chunk_no in range(first_chunk, min(nxt, chunk_count + 1)):
            per_chunk[chunk_no - 1] = samples_per
    deltas: list[int] = []
    for count, delta in stts:
        deltas.extend([delta] * count)
    if len(deltas) < sample_count:
        raise StructureError("stts shorter than sample count", stbl.offset)
    samples: list[Sample] = []
    dts = 0
    index = 0
    for chunk_index, chunk_offset in enumerate(chunk_offsets):
        offset = chunk_offset
        for _ in range(per_chunk[chunk_index]):
            if index >= sample_count:
                raise StructureError("stsc maps more samples than stsz", stbl.offset)
            size = sizes[index]
            samples.append(Sample(
                index=index, dts=dts, pts=dts + ctts[index],
                size=size, offset=offset, chunk_index=chunk_index,
            ))
            dts += deltas[index]
            offset += size
            index += 1
    if index != sample_count:
        raise StructureError(
            f"stsc maps {index} samples but stsz has {sample_count}", stbl.offset)
    return samples
