"""Fragmented MP4 moof/traf/tfhd/tfdt/trun scanning."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from pathlib import Path
from typing import Sequence

from .boxes import Box, find_boxes

MAX_TRUN_SAMPLES = 2_000_000


@dataclass(slots=True)
class TrunInfo:
    index: int
    version: int
    flags: int
    sample_count: int
    sample_durations: list[int] = field(default_factory=list)
    sample_sizes: list[int] = field(default_factory=list)
    data_start: int | None = None
    data_end: int | None = None
    relative_data_offset: int | None = None
    issues: list[str] = field(default_factory=list)

    @property
    def duration_ticks(self) -> int:
        return sum(self.sample_durations)

    @property
    def data_size(self) -> int:
        return sum(self.sample_sizes)


@dataclass(slots=True)
class TrackFragment:
    track_id: int
    tfdt: int | None
    base_data_offset: int | None
    default_base_is_moof: bool
    duration_ticks: int = 0
    sample_count: int = 0
    runs: list[TrunInfo] = field(default_factory=list)
    issues: list[str] = field(default_factory=list)

    @property
    def data_start(self) -> int | None:
        starts = [run.data_start for run in self.runs if run.data_start is not None]
        return min(starts) if starts else None

    @property
    def data_end(self) -> int | None:
        ends = [run.data_end for run in self.runs if run.data_end is not None]
        return max(ends) if ends else None

@dataclass(slots=True)
class MoofFragment:
    index: int
    file: Path
    moof_offset: int
    moof_size: int
    file_size: int
    sequence_number: int | None
    tracks: list[TrackFragment] = field(default_factory=list)
    issues: list[str] = field(default_factory=list)

    @property
    def duration_ticks(self) -> int:
        durations = [track.duration_ticks for track in self.tracks]
        return max(durations, default=0)

    @property
    def sample_count(self) -> int:
        return sum(track.sample_count for track in self.tracks)

    @property
    def tfdt(self) -> int | None:
        values = [track.tfdt for track in self.tracks if track.tfdt is not None]
        return min(values) if values else None

    @property
    def data_start(self) -> int | None:
        starts = [track.data_start for track in self.tracks if track.data_start is not None]
        return min(starts) if starts else None

    @property
    def data_end(self) -> int | None:
        ends = [track.data_end for track in self.tracks if track.data_end is not None]
        return max(ends) if ends else None

    @property
    def segment_end(self) -> int:
        return self.data_end or (self.moof_offset + self.moof_size)

    @property
    def fatal(self) -> bool:
        return bool(self.issues)


def parse_moofs(boxes: Sequence[Box], data: memoryview | bytes, file: Path) -> list[MoofFragment]:
    moof_boxes = sorted(find_boxes(boxes, "moof"), key=lambda box: box.start)
    fragments: list[MoofFragment] = []
    for index, moof in enumerate(moof_boxes, 1):
        fragment = MoofFragment(
            index, file, moof.start, moof.size, len(data), _sequence_number(moof, data)
        )
        trafs = [child for child in moof.children if child.type == "traf"]
        if not trafs:
            fragment.issues.append("moof has no traf")
        for traf in trafs:
            try:
                track = _parse_traf(traf, moof, data, len(data))
                fragment.tracks.append(track)
                fragment.issues.extend(track.issues)
                for run in track.runs:
                    fragment.issues.extend(run.issues)
            except (ValueError, struct.error) as exc:
                fragment.issues.append(f"traf at {traf.start}: {exc}")
        fragments.append(fragment)
    return fragments


def _sequence_number(moof: Box, data: memoryview | bytes) -> int | None:
    for child in moof.children:
        if child.type == "mfhd" and child.payload_size >= 8:
            return struct.unpack_from(">I", data, child.payload_start + 4)[0]
    return None


def _parse_traf(traf: Box, moof: Box, data: memoryview | bytes, file_size: int) -> TrackFragment:
    tfhd_box = next((child for child in traf.children if child.type == "tfhd"), None)
    if tfhd_box is None:
        raise ValueError("missing tfhd")
    tfhd = _parse_tfhd(tfhd_box, data)
    track = TrackFragment(
        track_id=tfhd["track_id"],
        tfdt=_parse_tfdt(traf, data),
        base_data_offset=tfhd["base_data_offset"],
        default_base_is_moof=tfhd["default_base_is_moof"],
    )
    base = moof.start if tfhd["default_base_is_moof"] else tfhd["base_data_offset"]
    if base is None:
        base = moof.start
    last_end: int | None = None
    trun_boxes = [child for child in traf.children if child.type == "trun"]
    for index, trun_box in enumerate(trun_boxes):
        run = _parse_trun(trun_box, index, data, tfhd)
        if run.relative_data_offset is not None:
            run.data_start = base + run.relative_data_offset
        elif index == 0 and tfhd["base_data_offset"] is not None and not tfhd["default_base_is_moof"]:
            run.data_start = base
        elif index == 0:
            run.data_start = moof.end
        else:
            run.data_start = last_end if last_end is not None else moof.end
        run.data_end = run.data_start + run.data_size
        if run.data_end > file_size:
            track.issues.append(
                f"trun {index} data ends at {run.data_end}, beyond file size {file_size}"
            )
        last_end = run.data_end
        track.runs.append(run)
        track.duration_ticks += run.duration_ticks
        track.sample_count += len(run.sample_durations)
    return track


def _parse_tfhd(box: Box, data: memoryview | bytes) -> dict[str, object]:
    if box.payload_size < 8:
        raise ValueError("truncated tfhd")
    flags = int.from_bytes(data[box.payload_start + 1:box.payload_start + 4], "big")
    cursor = box.payload_start + 4
    track_id = struct.unpack_from(">I", data, cursor)[0]
    cursor += 4
    base_data_offset: int | None = None
    if flags & 0x000001:
        if cursor + 8 > box.end:
            raise ValueError("truncated base_data_offset")
        base_data_offset = struct.unpack_from(">Q", data, cursor)[0]
        cursor += 8
    if flags & 0x000002:
        if cursor + 4 > box.end:
            raise ValueError("truncated sample_description_index")
        cursor += 4
    defaults: dict[str, int] = {}
    for bit, name in ((0x000008, "duration"), (0x000010, "size"), (0x000020, "flags")):
        if flags & bit:
            if cursor + 4 > box.end:
                raise ValueError(f"truncated default sample {name}")
            defaults[name] = struct.unpack_from(">I", data, cursor)[0]
            cursor += 4
    return {
        "track_id": track_id,
        "flags": flags,
        "base_data_offset": base_data_offset,
        "default_base_is_moof": bool(flags & 0x020000),
        "duration_is_empty": bool(flags & 0x010000),
        "default_duration": defaults.get("duration", 0),
        "default_size": defaults.get("size", 0),
    }


def _parse_tfdt(traf: Box, data: memoryview | bytes) -> int | None:
    box = next((child for child in traf.children if child.type == "tfdt"), None)
    if box is None or box.payload_size < 8:
        return None
    version = data[box.payload_start]
    if version == 0:
        return struct.unpack_from(">I", data, box.payload_start + 4)[0]
    if box.payload_size < 12:
        raise ValueError("truncated tfdt version 1")
    return struct.unpack_from(">Q", data, box.payload_start + 4)[0]


def _parse_trun(
    box: Box,
    index: int,
    data: memoryview | bytes,
    tfhd: dict[str, object],
) -> TrunInfo:
    if box.payload_size < 8:
        raise ValueError("truncated trun")
    version = data[box.payload_start]
    flags = int.from_bytes(data[box.payload_start + 1:box.payload_start + 4], "big")
    cursor = box.payload_start + 4
    sample_count = struct.unpack_from(">I", data, cursor)[0]
    cursor += 4
    if sample_count > MAX_TRUN_SAMPLES:
        raise ValueError(f"trun sample count {sample_count} exceeds {MAX_TRUN_SAMPLES}")
    relative_offset: int | None = None
    if flags & 0x000001:
        relative_offset = struct.unpack_from(">i", data, cursor)[0]
        cursor += 4
    if flags & 0x000004:
        cursor += 4
    record_size = 4 * sum(bool(flags & bit) for bit in (0x000100, 0x000200, 0x000400, 0x000800))
    available = (box.end - cursor) // record_size if record_size else sample_count
    actual_count = min(sample_count, available)
    run = TrunInfo(index, version, flags, sample_count, relative_data_offset=relative_offset)
    default_duration = int(tfhd.get("default_duration", 0))
    default_size = int(tfhd.get("default_size", 0))
    for _ in range(actual_count):
        duration = default_duration
        size = default_size
        if flags & 0x000100:
            duration = struct.unpack_from(">I", data, cursor)[0]
            cursor += 4
        if flags & 0x000200:
            size = struct.unpack_from(">I", data, cursor)[0]
            cursor += 4
        if flags & 0x000400:
            cursor += 4
        if flags & 0x000800:
            cursor += 4
        run.sample_durations.append(duration)
        run.sample_sizes.append(size)
    if actual_count != sample_count:
        run.issues.append(f"trun declares {sample_count} samples, only {actual_count} fit")
    return run
