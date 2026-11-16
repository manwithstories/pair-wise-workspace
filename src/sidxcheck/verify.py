"""Directory scan orchestration and cross-validation over shared structures."""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Sequence

from .boxes import ParseIssue, find_boxes, parse_file
from .moof import MoofFragment, parse_moofs
from .sidx import SidxBox, SidxError, parse_sidx
from .stbl import SampleTable, parse_sample_tables

_SIDECAR_SUFFIXES = {".json", ".sidx", ".txt", ".md", ".py", ".pyc", ".log"}


@dataclass(slots=True)
class FatalRecord:
    file: Path
    offset: int
    code: str
    message: str


@dataclass(slots=True)
class ScanResult:
    files: list[Path] = field(default_factory=list)
    file_sizes: dict[Path, int] = field(default_factory=dict)
    sidxes: list[SidxBox] = field(default_factory=list)
    moofs: list[MoofFragment] = field(default_factory=list)
    sample_tables: list[SampleTable] = field(default_factory=list)
    fatal: list[FatalRecord] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    @property
    def sidx_entries(self) -> int:
        return sum(len(sidx.sample_entries) for sidx in self.sidxes)

    @property
    def sidx_hierarchical_entries(self) -> int:
        return sum(len(sidx.entries) - len(sidx.sample_entries) for sidx in self.sidxes)

    @property
    def boundary_count(self) -> int:
        return len(self.moofs)

    @property
    def sample_count(self) -> int:
        table_count = sum(table.sample_count for table in self.sample_tables)
        return table_count if table_count else sum(moof.sample_count for moof in self.moofs)

    @property
    def duration_ms(self) -> int:
        table_duration = max((table.duration_ms for table in self.sample_tables), default=0)
        if table_duration:
            return table_duration
        if not self.moofs:
            return 0
        timescale = next((sidx.timescale for sidx in self.sidxes if sidx.timescale), 1000)
        ticks = sum(moof.duration_ticks for moof in self.moofs)
        return round(ticks * 1000 / timescale) if timescale else 0

    @property
    def fatal_count(self) -> int:
        return len(self.fatal)


@dataclass(slots=True)
class OffsetMismatch:
    file: Path
    entry_index: int
    declared_start: int
    actual_start: int
    start_delta: int
    declared_end: int
    actual_end: int
    end_delta: int

    @property
    def max_abs_delta(self) -> int:
        return max(abs(self.start_delta), abs(self.end_delta))


@dataclass(slots=True)
class VerificationResult:
    scan: ScanResult
    offset_mismatches: list[OffsetMismatch] = field(default_factory=list)
    validation_errors: list[str] = field(default_factory=list)
    max_drift_ticks: int = 0
    offset_max_drift: int = 0

    @property
    def offset_mismatch(self) -> int:
        return len(self.offset_mismatches)

    @property
    def inconsistent(self) -> bool:
        return bool(self.offset_mismatches or self.validation_errors or self.max_drift_ticks)


def scan_path(input_path: str | Path) -> ScanResult:
    target = Path(input_path)
    if not target.exists():
        raise FileNotFoundError(f"input does not exist: {target}")
    files = _collect_files(target)
    result = ScanResult(files=files)
    for path in files:
        _scan_file(path, result)
    return result


def verify_scan(scan: ScanResult) -> VerificationResult:
    result = VerificationResult(scan)
    if scan.moofs and not scan.sidxes:
        result.validation_errors.append("no sidx found for fragmented media")
    for sidx in scan.sidxes:
        result.validation_errors.extend(f"{sidx.file}: {issue}" for issue in sidx.issues)
    moofs_by_file: dict[Path, list[MoofFragment]] = defaultdict(list)
    for fragment in sorted(scan.moofs, key=lambda item: (str(item.file), item.moof_offset)):
        moofs_by_file[fragment.file].append(fragment)
    for sidx in scan.sidxes:
        _check_sidx_offsets(sidx, moofs_by_file[sidx.file], scan, result)
        for table in scan.sample_tables:
            if (
                table.file == sidx.file
                and table.timescale
                and sidx.timescale
                and table.timescale != sidx.timescale
            ):
                result.validation_errors.append(
                    f"{sidx.file}: sidx timescale {sidx.timescale} differs from mdhd {table.timescale}"
                )
    _check_timing(scan, moofs_by_file, result)
    _check_sample_tables(scan, result)
    return result


def exit_code(result: VerificationResult) -> int:
    if result.scan.fatal_count:
        return 3
    if result.inconsistent:
        return 1
    return 0


def _collect_files(target: Path) -> list[Path]:
    if target.is_file():
        return [target]
    files = []
    for path in sorted(target.rglob("*")):
        if not path.is_file() or any(part.startswith(".") for part in path.relative_to(target).parts):
            continue
        if path.suffix.lower() in _SIDECAR_SUFFIXES:
            continue
        files.append(path)
    return files


def _scan_file(path: Path, result: ScanResult) -> None:
    with parse_file(path) as parsed:
        result.file_sizes[path] = len(parsed.data)
        for issue in parsed.issues:
            _record_issue(path, issue, result)
        for box in find_boxes(parsed.boxes, "sidx"):
            try:
                result.sidxes.append(parse_sidx(box, parsed.data, file=path))
            except SidxError as exc:
                result.fatal.append(FatalRecord(path, box.start, "invalid_sidx", str(exc)))
        fragments = parse_moofs(parsed.boxes, parsed.data, path)
        result.moofs.extend(fragments)
        for fragment in fragments:
            for message in fragment.issues:
                result.fatal.append(
                    FatalRecord(path, fragment.moof_offset, "moof_parse", message)
                )
        result.sample_tables.extend(parse_sample_tables(parsed.boxes, parsed.data, path))


def _record_issue(path: Path, issue: ParseIssue, result: ScanResult) -> None:
    if issue.severity == "fatal":
        result.fatal.append(FatalRecord(path, issue.offset, issue.code, issue.message))
    else:
        result.notes.append(f"{path}:{issue.offset}: {issue.code}: {issue.message}")


def _check_sidx_offsets(
    sidx: SidxBox,
    moofs: Sequence[MoofFragment],
    scan: ScanResult,
    result: VerificationResult,
) -> None:
    file_size = scan.file_sizes.get(sidx.file or Path(), 0)
    starts = [moof.moof_offset for moof in moofs]
    cursor = sidx.anchor_offset + sidx.first_offset
    if sidx.entries and cursor >= file_size:
        result.validation_errors.append(
            f"{sidx.file}: sidx first_offset lands at {cursor}, beyond {file_size}"
        )
    if sidx.declared_reference_count != len(sidx.entries):
        result.validation_errors.append(
            f"{sidx.file}: reference_count={sidx.declared_reference_count}, parsed={len(sidx.entries)}"
        )
    sample_index = 0
    for entry in sidx.entries:
        declared_end = cursor + entry.referenced_size
        if not entry.is_hierarchical:
            if sample_index >= len(starts):
                result.validation_errors.append(
                    f"{sidx.file}: sidx entry {entry.index} has no matching moof"
                )
            else:
                actual_start = starts[sample_index]
                actual_end = (
                    starts[sample_index + 1]
                    if sample_index + 1 < len(starts)
                    else moofs[sample_index].segment_end
                )
                mismatch = OffsetMismatch(
                    sidx.file or Path(),
                    entry.index,
                    cursor,
                    actual_start,
                    cursor - actual_start,
                    declared_end,
                    actual_end,
                    declared_end - actual_end,
                )
                if mismatch.start_delta or mismatch.end_delta:
                    result.offset_mismatches.append(mismatch)
                sample_index += 1
        cursor = declared_end
    if sample_index != len(starts):
        result.validation_errors.append(
            f"{sidx.file}: reference_count={sidx.declared_reference_count} "
            f"does not match moof_count={len(starts)} (parsed={sample_index})"
        )
    result.offset_max_drift = max(
        (mismatch.max_abs_delta for mismatch in result.offset_mismatches),
        default=result.offset_max_drift,
    )


def _check_timing(
    scan: ScanResult,
    moofs_by_file: dict[Path, list[MoofFragment]],
    result: VerificationResult,
) -> None:
    drift = 0
    for sidx in scan.sidxes:
        moofs = moofs_by_file[sidx.file]
        timestamp = sidx.earliest_presentation_time
        for entry, fragment in zip(sidx.sample_entries, moofs):
            drift = max(drift, abs(entry.subsegment_duration - fragment.duration_ticks))
            if fragment.tfdt is not None:
                drift = max(drift, abs(timestamp - fragment.tfdt))
            timestamp += entry.subsegment_duration
        previous: MoofFragment | None = None
        for fragment in moofs:
            if previous is not None and previous.tfdt is not None and fragment.tfdt is not None:
                delta = fragment.tfdt - previous.tfdt
                drift = max(drift, abs(delta - previous.duration_ticks))
            previous = fragment
    result.max_drift_ticks = drift


def _check_sample_tables(scan: ScanResult, result: VerificationResult) -> None:
    for table in scan.sample_tables:
        result.validation_errors.extend(f"{table.file}: {issue}" for issue in table.issues)
    populated = [table for table in scan.sample_tables if table.sample_count]
    if not populated or not scan.moofs:
        return
    table_samples = sum(table.sample_count for table in populated)
    moof_samples = sum(moof.sample_count for moof in scan.moofs)
    if table_samples != moof_samples:
        result.validation_errors.append(
            f"sample tables contain {table_samples} samples, moofs contain {moof_samples}"
        )
    if len(populated) == 1:
        table = populated[0]
        moof_ticks = sum(moof.duration_ticks for moof in scan.moofs)
        if table.total_duration != moof_ticks:
            result.validation_errors.append(
                f"stts total {table.total_duration} ticks differs from moof total {moof_ticks}"
            )
        result.max_drift_ticks = max(
            result.max_drift_ticks, abs(table.total_duration - moof_ticks)
        )
        moofs = [moof for moof in scan.moofs if moof.file == table.file]
        chunk_durations = table.chunk_durations
        if len(chunk_durations) == len(moofs) and all(moof.tfdt is not None for moof in moofs):
            timestamp = moofs[0].tfdt or 0
            for fragment, duration in zip(moofs, chunk_durations):
                result.max_drift_ticks = max(
                    result.max_drift_ticks, abs(timestamp - (fragment.tfdt or 0))
                )
                timestamp += duration
