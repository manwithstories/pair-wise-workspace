"""Cross-validation: sample offsets vs mdat, rebuilt index vs original sidx."""
from __future__ import annotations

from dataclasses import dataclass, field

from .boxes import build_tree, mdat_ranges
from .reader import ByteCursor, StructureError
from .rebuild import RebuiltIndex, collect_fragments, rebuild_index
from .sidx import SidxReference, expand_sidx, parse_sidx
from .stbl import Sample, expand_samples

MAX_LISTED_VIOLATIONS = 10


@dataclass
class OffsetCheck:
    total: int
    in_range: int
    violation_count: int
    violations: list[Sample] = field(default_factory=list)  # first 10 only

    @property
    def status(self) -> str:
        return "pass" if self.violation_count == 0 else "fail"


@dataclass
class IndexComparison:
    entry_count_expected: int
    entry_count_actual: int
    offset_delta_total: int
    duration_delta_total: int
    first_mismatch: dict | None

    @property
    def entry_count_delta(self) -> int:
        return self.entry_count_actual - self.entry_count_expected

    @property
    def verdict(self) -> str:
        ok = (self.entry_count_delta == 0
              and self.offset_delta_total == 0
              and self.duration_delta_total == 0)
        return "PASS" if ok else "FAIL"


@dataclass
class FileReport:
    path: str
    sample_count: int
    sidx_entries: int
    offset_check: OffsetCheck
    rebuild_boundaries: list[int]
    comparison: IndexComparison

    @property
    def verdict(self) -> str:
        if self.offset_check.violation_count:
            return "FAIL"
        return self.comparison.verdict


def check_sample_offsets(samples: list[Sample],
                         ranges: list[tuple[int, int]]) -> OffsetCheck:
    """Each sample's byte span must fall inside one mdat payload range."""
    violations: list[Sample] = []
    violation_count = 0
    for sample in samples:
        end = sample.offset + sample.size
        inside = any(start <= sample.offset and end <= stop
                     for start, stop in ranges)
        if not inside:
            violation_count += 1
            if len(violations) < MAX_LISTED_VIOLATIONS:
                violations.append(sample)
    return OffsetCheck(
        total=len(samples),
        in_range=len(samples) - violation_count,
        violation_count=violation_count,
        violations=violations,
    )


def compare_indexes(rebuilt: RebuiltIndex,
                    references: list[SidxReference]) -> IndexComparison:
    """Entry-by-entry comparison of rebuilt boundaries against the sidx."""
    offset_delta_total = 0
    duration_delta_total = 0
    first_mismatch: dict | None = None
    count = min(len(rebuilt.entries), len(references))
    for idx in range(count):
        entry = rebuilt.entries[idx]
        ref = references[idx]
        offset_delta = entry.byte_start - ref.byte_start
        duration_delta = entry.subsegment_duration - ref.subsegment_duration
        size_delta = entry.reference_size - ref.referenced_size
        offset_delta_total += abs(offset_delta) + abs(size_delta)
        duration_delta_total += abs(duration_delta)
        if first_mismatch is None and (
                offset_delta or duration_delta or size_delta):
            first_mismatch = {
                "index": idx,
                "expected_offset": ref.byte_start,
                "actual_offset": entry.byte_start,
                "offset_delta": offset_delta,
                "expected_duration": ref.subsegment_duration,
                "actual_duration": entry.subsegment_duration,
                "duration_delta": duration_delta,
                "size_delta": size_delta,
            }
    if first_mismatch is None and len(rebuilt.entries) != len(references):
        first_mismatch = {
            "index": count,
            "reason": "entry count differs",
            "expected_entries": len(references),
            "actual_entries": len(rebuilt.entries),
        }
    return IndexComparison(
        entry_count_expected=len(references),
        entry_count_actual=len(rebuilt.entries),
        offset_delta_total=offset_delta_total,
        duration_delta_total=duration_delta_total,
        first_mismatch=first_mismatch,
    )


def validate_file(path) -> FileReport:
    """Parse once, then run offset check and index comparison."""
    cursor = ByteCursor.open(path)
    root = build_tree(cursor)
    stbl = root.find_one("moov/trak/mdia/minf/stbl")
    if stbl is None:
        raise StructureError("no moov/trak/mdia/minf/stbl", 0)
    samples = expand_samples(cursor, stbl)
    offset_check = check_sample_offsets(samples, mdat_ranges(root))
    sidx_box = root.find_one("sidx")
    if sidx_box is None:
        raise StructureError("no top-level sidx box", 0)
    sidx = parse_sidx(cursor, sidx_box)
    references = expand_sidx(cursor, sidx)
    fragments = collect_fragments(cursor, root)
    rebuilt = rebuild_index(fragments, samples, sidx.box_end)
    comparison = compare_indexes(rebuilt, references)
    return FileReport(
        path=str(path),
        sample_count=len(samples),
        sidx_entries=len(references),
        offset_check=offset_check,
        rebuild_boundaries=rebuilt.boundaries,
        comparison=comparison,
    )
