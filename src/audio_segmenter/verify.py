"""Invariant checks for detection results.

The checks answer a single question -- "is this result internally
consistent?" -- and are shared by the ``verify`` subcommand, the demo script
and the unit tests.  Every failure names the file and the reason.

Invariants, per file:

1. sample rate is positive and matches every region
2. segments are ordered, non-empty and never overlap
3. silences are ordered, non-empty and never overlap
4. segments and silences are disjoint in sample space
5. segment + silence sample counts equal the file's total
6. every boundary lies inside ``[0, total_samples)``
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional, Sequence

from .model import DetectionResult, FileAnalysis


@dataclass
class VerificationIssue:
    path: str
    reason: str

    def to_dict(self) -> dict[str, str]:
        return {"path": self.path, "reason": self.reason}

    def __str__(self) -> str:
        return f"{self.path}: {self.reason}"


@dataclass
class VerificationReport:
    """Counts only files that were actually verified.

    Files whose analysis failed are reported separately as ``skipped``; they
    were never checked, so they must not inflate ``checked_files`` and let a
    run of nothing but bad inputs look like a pass.
    """

    checked_files: int = 0
    skipped_files: int = 0
    issues: list[VerificationIssue] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        return not self.issues

    def add(self, path: str, reason: str) -> None:
        self.issues.append(VerificationIssue(path=path, reason=reason))

    def to_dict(self) -> dict:
        return {
            "checked_files": self.checked_files,
            "skipped_files": self.skipped_files,
            "passed": self.passed,
            "issue_count": len(self.issues),
            "issues": [i.to_dict() for i in self.issues],
        }


def _check_ordering(report: VerificationReport, path: str, label: str, regions) -> bool:
    ok = True
    previous_end = 0
    for pos, region in enumerate(regions):
        if region.end_sample <= region.start_sample:
            report.add(path, f"{label}[{pos}] is empty or inverted")
            ok = False
        if region.start_sample < previous_end:
            report.add(
                path,
                f"{label}[{pos}] starts at {region.start_sample},"
                f" before previous end {previous_end}",
            )
            ok = False
        previous_end = max(previous_end, region.end_sample)
    return ok


def verify_result(result: DetectionResult) -> VerificationReport:
    report = VerificationReport(checked_files=1)
    path = result.path or "<unnamed>"
    if result.sample_rate <= 0:
        report.add(path, f"invalid sample rate {result.sample_rate}")
        return report
    if result.total_samples <= 0:
        report.add(path, "file has no samples")
        return report

    for pos, seg in enumerate(result.segments):
        if seg.sample_rate != result.sample_rate:
            report.add(path, f"segments[{pos}] sample rate mismatch")
        if seg.start_sample < 0 or seg.end_sample > result.total_samples:
            report.add(path, f"segments[{pos}] lies outside the file")
        if seg.sample_count != seg.end_sample - seg.start_sample:
            report.add(path, f"segments[{pos}] sample count is inconsistent")
    for pos, sil in enumerate(result.silences):
        if sil.sample_rate != result.sample_rate:
            report.add(path, f"silences[{pos}] sample rate mismatch")
        if sil.start_sample < 0 or sil.end_sample > result.total_samples:
            report.add(path, f"silences[{pos}] lies outside the file")
        if sil.sample_count != sil.end_sample - sil.start_sample:
            report.add(path, f"silences[{pos}] sample count is inconsistent")

    _check_ordering(report, path, "segments", result.segments)
    _check_ordering(report, path, "silences", result.silences)

    # Disjointness: walk both lists in sample order.
    merged = sorted(
        [(s.start_sample, s.end_sample, "segment") for s in result.segments]
        + [(s.start_sample, s.end_sample, "silence") for s in result.silences]
    )
    for (a_start, a_end, a_kind), (b_start, b_end, b_kind) in zip(merged, merged[1:]):
        if b_start < a_end:
            report.add(
                path,
                f"{a_kind}[{a_start}:{a_end}] overlaps {b_kind}[{b_start}:{b_end}]",
            )

    total = result.total_segment_samples + result.total_silence_samples
    if total != result.total_samples:
        report.add(
            path,
            f"duration not conserved: segments+silences={total}"
            f" total_samples={result.total_samples}",
        )
    return report


def verify_analyses(analyses: Sequence[FileAnalysis]) -> VerificationReport:
    """Verify every successfully analysed file.

    Failed inputs are counted as skipped, never as checked: verification
    answers "are these results sound?", and an unreadable file has no results
    to sound out.
    """

    report = VerificationReport()
    for analysis in analyses:
        if analysis.status != "ok" or analysis.result is None:
            report.skipped_files += 1
            if analysis.status == "ok":
                report.add(analysis.path, "analysis is marked ok but has no result")
            continue
        report.checked_files += 1
        partial = verify_result(analysis.result)
        report.issues.extend(partial.issues)
    return report


def verify_summary(
    batch_files: Sequence[dict], results: Sequence[DetectionResult]
) -> VerificationReport:
    """Verify results loaded from sqlite against their file summaries."""

    report = VerificationReport()
    by_path = {r.path: r for r in results}
    for row in batch_files:
        path = row.get("path", "<unknown>")
        report.checked_files += 1
        result = by_path.get(path)
        if result is None:
            report.add(path, "no detection result available")
            continue
        if row.get("sample_rate") is not None and row["sample_rate"] != result.sample_rate:
            report.add(path, "sample rate in database differs from result")
        partial = verify_result(result)
        report.issues.extend(partial.issues)
    return report
