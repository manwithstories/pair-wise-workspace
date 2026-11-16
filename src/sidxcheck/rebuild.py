"""Rebuild a sidx from measured moof positions and durations."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Sequence

from .moof import MoofFragment
from .sidx import SidxBox, SidxEntry, encode_sidx
from .stbl import SampleTable


@dataclass(slots=True)
class RebuildResult:
    data: bytes
    boundary_count: int
    max_drift_ticks: int
    first_offset: int
    earliest_presentation_time: int
    duration_ticks: int
    entries: list[SidxEntry] = field(default_factory=list)
    issues: list[str] = field(default_factory=list)


def rebuild_sidx(
    fragments: Sequence[MoofFragment],
    *,
    sidxes: Sequence[SidxBox] = (),
    sample_tables: Sequence[SampleTable] = (),
) -> RebuildResult:
    ordered = sorted(fragments, key=lambda item: (str(item.file), item.moof_offset))
    if not ordered:
        return RebuildResult(b"", 0, 0, 0, 0, 0, issues=["no moof boundaries found"])

    first = ordered[0]
    timescale = _choose_timescale(sidxes, sample_tables)
    earliest = first.tfdt if first.tfdt is not None else 0
    existing = _matching_sidx(first, sidxes)
    anchor = existing.anchor_offset if existing is not None else 0
    first_offset = max(0, first.moof_offset - anchor)
    reference_id = _reference_id(existing, sample_tables)

    by_file: dict[Path, list[MoofFragment]] = {}
    for fragment in ordered:
        by_file.setdefault(fragment.file, []).append(fragment)
    next_start: dict[int, int] = {}
    for group in by_file.values():
        for current, following in zip(group, group[1:]):
            next_start[id(current)] = following.moof_offset

    entries: list[SidxEntry] = []
    issues: list[str] = []
    for index, fragment in enumerate(ordered):
        end = next_start.get(id(fragment), fragment.segment_end)
        referenced_size = max(0, end - fragment.moof_offset)
        duration = max(0, fragment.duration_ticks)
        if referenced_size > 0x7FFFFFFF:
            issues.append(f"entry {index} referenced_size exceeds 31 bits")
        if duration > 0xFFFFFFFF:
            issues.append(f"entry {index} duration exceeds 32 bits")
        entries.append(
            SidxEntry(
                index=index,
                reference_type=0,
                referenced_size=min(referenced_size, 0x7FFFFFFF),
                subsegment_duration=min(duration, 0xFFFFFFFF),
            )
        )

    drift = max(
        (abs(entry.subsegment_duration - fragment.duration_ticks)
         for entry, fragment in zip(entries, ordered)),
        default=0,
    )
    data = encode_sidx(
        reference_id=reference_id,
        timescale=timescale,
        earliest_presentation_time=earliest,
        first_offset=first_offset,
        entries=entries,
    )
    return RebuildResult(
        data=data,
        boundary_count=len(entries),
        max_drift_ticks=drift,
        first_offset=first_offset,
        earliest_presentation_time=earliest,
        duration_ticks=sum(entry.subsegment_duration for entry in entries),
        entries=entries,
        issues=issues,
    )


def _matching_sidx(fragment: MoofFragment, sidxes: Sequence[SidxBox]) -> SidxBox | None:
    candidates = [sidx for sidx in sidxes if sidx.file == fragment.file]
    candidates = [sidx for sidx in candidates if sidx.box_offset < fragment.moof_offset]
    return max(candidates, key=lambda sidx: sidx.box_offset, default=None)


def _choose_timescale(sidxes: Sequence[SidxBox], tables: Sequence[SampleTable]) -> int:
    for table in tables:
        if table.timescale > 0:
            return table.timescale
    for sidx in sidxes:
        if sidx.timescale > 0:
            return sidx.timescale
    return 1000


def _reference_id(existing: SidxBox | None, tables: Sequence[SampleTable]) -> int:
    if existing is not None:
        return existing.reference_id
    for table in tables:
        if table.track_id is not None:
            return table.track_id
    return 1
