"""Sample table parsing and sample-to-chunk mapping."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator, Sequence

from .boxes import Box, find_boxes

MAX_SAMPLE_MAP = 2_000_000


@dataclass(slots=True)
class SttsEntry:
    sample_count: int
    sample_delta: int


@dataclass(slots=True)
class StscEntry:
    first_chunk: int
    samples_per_chunk: int
    sample_description_index: int


@dataclass(slots=True)
class SampleLocation:
    index: int
    chunk_index: int
    offset: int
    size: int
    duration: int


@dataclass(slots=True)
class SampleTable:
    file: Path
    track_id: int | None
    timescale: int
    stts: list[SttsEntry] = field(default_factory=list)
    stsc: list[StscEntry] = field(default_factory=list)
    sample_count: int = 0
    constant_sample_size: int = 0
    sample_sizes: list[int] = field(default_factory=list)
    chunk_offsets: list[int] = field(default_factory=list)
    sync_samples: list[int] | None = None
    sample_locations: list[SampleLocation] = field(default_factory=list)
    issues: list[str] = field(default_factory=list)

    @property
    def total_duration(self) -> int:
        return sum(entry.sample_count * entry.sample_delta for entry in self.stts)

    @property
    def duration_ms(self) -> int:
        if not self.timescale:
            return 0
        return round(self.total_duration * 1000 / self.timescale)

    def size_of(self, index: int) -> int:
        if self.constant_sample_size:
            return self.constant_sample_size
        return self.sample_sizes[index] if index < len(self.sample_sizes) else 0

    @property
    def chunk_durations(self) -> list[int]:
        if len(self.sample_locations) != self.sample_count:
            return []
        durations = [0] * len(self.chunk_offsets)
        for location in self.sample_locations:
            if 1 <= location.chunk_index <= len(durations):
                durations[location.chunk_index - 1] += location.duration
        return durations


def parse_sample_tables(
    boxes: Sequence[Box], data: memoryview | bytes, file: Path
) -> list[SampleTable]:
    tables: list[SampleTable] = []
    for trak in find_boxes(boxes, "trak"):
        track_id = _track_id(trak, data)
        timescale = _timescale(trak, data)
        for stbl in find_boxes(trak.children, "stbl"):
            tables.append(parse_stbl(stbl, data, file=file, track_id=track_id, timescale=timescale))
    return tables


def parse_stbl(
    stbl: Box,
    data: memoryview | bytes,
    *,
    file: Path,
    track_id: int | None = None,
    timescale: int = 0,
) -> SampleTable:
    table = SampleTable(file=file, track_id=track_id, timescale=timescale)
    boxes = {box.type: box for box in stbl.children}
    try:
        if "stts" in boxes:
            table.stts = _parse_stts(boxes["stts"], data)
        if "stsc" in boxes:
            table.stsc = _parse_stsc(boxes["stsc"], data)
        if "stsz" in boxes:
            count, constant, sizes = _parse_stsz(boxes["stsz"], data)
            table.sample_count, table.constant_sample_size, table.sample_sizes = count, constant, sizes
        chunk_box = boxes.get("stco") or boxes.get("co64")
        if chunk_box is not None:
            table.chunk_offsets = _parse_chunk_offsets(chunk_box, data)
        if "stss" in boxes:
            table.sync_samples = _parse_stss(boxes["stss"], data)
    except (ValueError, struct.error) as exc:
        table.issues.append(str(exc))
    _validate_counts(table)
    _build_sample_map(table)
    return table


def _parse_stts(box: Box, data: memoryview | bytes) -> list[SttsEntry]:
    _require(box, 8, "stts")
    count = struct.unpack_from(">I", data, box.payload_start + 4)[0]
    available = (box.payload_size - 8) // 8
    actual = min(count, available)
    entries = [
        SttsEntry(*struct.unpack_from(">II", data, box.payload_start + 8 + i * 8))
        for i in range(actual)
    ]
    if actual != count:
        raise ValueError(f"stts declares {count} entries, only {actual} fit")
    return entries


def _parse_stsc(box: Box, data: memoryview | bytes) -> list[StscEntry]:
    _require(box, 8, "stsc")
    count = struct.unpack_from(">I", data, box.payload_start + 4)[0]
    available = (box.payload_size - 8) // 12
    actual = min(count, available)
    entries = [
        StscEntry(*struct.unpack_from(">III", data, box.payload_start + 8 + i * 12))
        for i in range(actual)
    ]
    if actual != count:
        raise ValueError(f"stsc declares {count} entries, only {actual} fit")
    return entries


def _parse_stsz(box: Box, data: memoryview | bytes) -> tuple[int, int, list[int]]:
    _require(box, 12, "stsz")
    sample_size, count = struct.unpack_from(">II", data, box.payload_start + 4)
    if sample_size:
        return count, sample_size, []
    available = (box.payload_size - 12) // 4
    actual = min(count, available)
    sizes = [
        struct.unpack_from(">I", data, box.payload_start + 12 + i * 4)[0]
        for i in range(actual)
    ]
    if actual != count:
        raise ValueError(f"stsz declares {count} samples, only {actual} sizes fit")
    return count, 0, sizes


def _parse_chunk_offsets(box: Box, data: memoryview | bytes) -> list[int]:
    _require(box, 8, box.type)
    count = struct.unpack_from(">I", data, box.payload_start + 4)[0]
    width = 8 if box.type == "co64" else 4
    fmt = ">Q" if width == 8 else ">I"
    available = (box.payload_size - 8) // width
    actual = min(count, available)
    offsets = [
        struct.unpack_from(fmt, data, box.payload_start + 8 + i * width)[0]
        for i in range(actual)
    ]
    if actual != count:
        raise ValueError(f"{box.type} declares {count} offsets, only {actual} fit")
    return offsets


def _parse_stss(box: Box, data: memoryview | bytes) -> list[int]:
    _require(box, 8, "stss")
    count = struct.unpack_from(">I", data, box.payload_start + 4)[0]
    available = (box.payload_size - 8) // 4
    actual = min(count, available)
    samples = [
        struct.unpack_from(">I", data, box.payload_start + 8 + i * 4)[0]
        for i in range(actual)
    ]
    if actual != count:
        raise ValueError(f"stss declares {count} entries, only {actual} fit")
    return samples


def _validate_counts(table: SampleTable) -> None:
    stts_count = sum(entry.sample_count for entry in table.stts)
    if table.stts and stts_count != table.sample_count:
        table.issues.append(f"stts covers {stts_count} samples, stsz declares {table.sample_count}")
    if not table.stts and table.sample_count:
        table.issues.append("missing stts while stsz declares samples")
    if table.sample_count and not table.stsc:
        table.issues.append("missing stsc while stsz declares samples")
    for previous, current in zip(table.chunk_offsets, table.chunk_offsets[1:]):
        if current < previous:
            table.issues.append(f"chunk offsets are not monotonic: {previous} then {current}")
            break


def _chunk_map(table: SampleTable) -> dict[int, int]:
    mapping: dict[int, int] = {}
    chunk_count = len(table.chunk_offsets)
    if not table.stsc:
        return mapping
    if table.stsc[0].first_chunk != 1:
        table.issues.append(f"stsc starts at chunk {table.stsc[0].first_chunk}, not 1")
    for index, entry in enumerate(table.stsc):
        end = table.stsc[index + 1].first_chunk - 1 if index + 1 < len(table.stsc) else chunk_count
        end = min(end, chunk_count)
        if entry.first_chunk < 1 or entry.first_chunk > end:
            table.issues.append(f"invalid stsc chunk range {entry.first_chunk}..{end}")
            continue
        for chunk in range(entry.first_chunk, end + 1):
            mapping[chunk] = entry.samples_per_chunk
    missing = [chunk for chunk in range(1, chunk_count + 1) if chunk not in mapping]
    if missing:
        table.issues.append(f"stsc does not cover {len(missing)} chunk(s), first={missing[0]}")
    return mapping


def _build_sample_map(table: SampleTable) -> None:
    mapping = _chunk_map(table)
    mapped_slots = sum(mapping.values())
    if table.sample_count and mapped_slots != table.sample_count:
        table.issues.append(
            f"stsc maps {mapped_slots} sample slots, stsz declares {table.sample_count}"
        )
    if not table.chunk_offsets or not table.sample_count:
        return
    limit = min(table.sample_count, MAX_SAMPLE_MAP)
    if table.sample_count > MAX_SAMPLE_MAP:
        table.issues.append(f"sample map limited to {MAX_SAMPLE_MAP} entries")
    durations = _duration_stream(table.stts, limit)
    sample_index = 0
    for chunk_index, chunk_offset in enumerate(table.chunk_offsets, 1):
        cursor = chunk_offset
        for _ in range(mapping.get(chunk_index, 0)):
            if sample_index >= limit:
                return
            duration = next(durations, 0)
            size = table.size_of(sample_index)
            table.sample_locations.append(
                SampleLocation(sample_index, chunk_index, cursor, size, duration)
            )
            cursor += size
            sample_index += 1


def _duration_stream(entries: Sequence[SttsEntry], limit: int) -> Iterator[int]:
    emitted = 0
    for entry in entries:
        for _ in range(entry.sample_count):
            if emitted >= limit:
                return
            emitted += 1
            yield entry.sample_delta


def _track_id(trak: Box, data: memoryview | bytes) -> int | None:
    box = next((child for child in trak.children if child.type == "tkhd"), None)
    if box is None or box.payload_size < 16:
        return None
    version = data[box.payload_start]
    offset = 20 if version == 1 else 12
    if box.payload_size < offset + 4:
        return None
    return struct.unpack_from(">I", data, box.payload_start + offset)[0]


def _timescale(trak: Box, data: memoryview | bytes) -> int:
    box = next((child for child in trak.children if child.type == "mdia"), None)
    if box is None:
        return 0
    mdhd = next((child for child in box.children if child.type == "mdhd"), None)
    if mdhd is None:
        return 0
    version = data[mdhd.payload_start]
    offset = 20 if version == 1 else 12
    if mdhd.payload_size < offset + 4:
        return 0
    return struct.unpack_from(">I", data, mdhd.payload_start + offset)[0]


def _require(box: Box, size: int, name: str) -> None:
    if box.payload_size < size:
        raise ValueError(f"truncated {name}")
