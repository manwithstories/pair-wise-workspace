"""Round-trip parsing metrics and per-segment SHA-256 verification.

The verifier consumes the same :class:`~archive_media.manifest_core.SegmentPlan`
and error model as the rest of the pipeline, so its numbers can never be based
on a different interpretation of the manifest than the recovery itself.

Three checks are provided:

* :func:`roundtrip_consistency` re-serializes the plan and parses it again,
  comparing representation / sequence / raw references and the exact rational
  timing; the returned score is the fraction of matching segment fields,
* :func:`verify_segment_hashes` recomputes output WAV hashes and compares them
  with the recorded values -- an unchanged repair run must yield zero
  mismatches, while changed inputs are reported as
  :data:`~archive_media.errors.Code.HASH_MISMATCH` with the locator,
* :func:`input_fingerprint` hashes the manifest text plus every referenced
  segment so a re-run can state explicitly whether its inputs changed.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from pathlib import Path

from .errors import Code, Diagnostic, Locator
from .manifest import render_plan
from .manifest_core import SegmentPlan, parse_manifest
from .timeline import rational


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1 << 16), b""):
            digest.update(block)
    return digest.hexdigest()


def input_fingerprint(plan: SegmentPlan) -> str:
    """Stable fingerprint of the manifest text and every referenced file."""
    digest = hashlib.sha256()
    digest.update(plan.kind.encode("utf-8"))
    digest.update(b"\0")
    digest.update(plan.source_text.encode("utf-8"))
    for segment in sorted(plan.segments, key=lambda s: s.sequence):
        digest.update(segment.raw_ref.encode("utf-8"))
        digest.update(b"\0")
        if segment.resolved_path.exists():
            digest.update(_file_sha256(segment.resolved_path).encode("ascii"))
        digest.update(b"\0")
    return digest.hexdigest()


def _segment_field_tuple(plan: SegmentPlan) -> list[tuple]:
    return [
        (
            s.representation,
            s.sequence,
            s.raw_ref,
            str(s.start),
            str(s.duration),
            str(s.time_base),
        )
        for s in sorted(plan.segments, key=lambda s: s.sequence)
    ]


def roundtrip_consistency(plan: SegmentPlan) -> tuple[float, list[Diagnostic]]:
    """Re-render and re-parse; return the matched-field fraction and diagnostics."""
    rendered = render_plan(plan)
    sibling = plan.manifest_path.with_name(
        plan.manifest_path.stem + ".roundtrip" + plan.manifest_path.suffix
    )
    sibling.write_text(rendered, encoding="utf-8")
    try:
        reparsed = parse_manifest(sibling, plan.root)
    finally:
        sibling.unlink(missing_ok=True)
    before = _segment_field_tuple(plan)
    after = _segment_field_tuple(reparsed)
    diagnostics: list[Diagnostic] = []
    if len(before) != len(after):
        diagnostics.append(
            Diagnostic(
                Code.INPUT_CHANGED,
                "round-trip changed the number of segments",
                Locator(manifest_path=str(plan.manifest_path)),
                {"before": len(before), "after": len(after)},
            )
        )
    fields_per_segment = max(1, len(before[0]) if before else 1)
    total = max(1, len(before)) * fields_per_segment
    matched = 0
    for index, (left, right) in enumerate(zip(before, after)):
        if left == right:
            matched += fields_per_segment
            continue
        for field_name, left_value, right_value in zip(
            ("representation", "sequence", "raw_ref", "start", "duration",
             "time_base"),
            left,
            right,
        ):
            if left_value == right_value:
                matched += 1
            else:
                diagnostics.append(
                    Diagnostic(
                        Code.INPUT_CHANGED,
                        f"round-trip mismatch on field {field_name}",
                        Locator(
                            manifest_path=str(plan.manifest_path),
                            sequence=index,
                            segment_ref=left[2],
                        ),
                        {"before": left_value, "after": right_value},
                    )
                )
    return matched / total, diagnostics


@dataclass
class HashReport:
    count: int
    mismatches: int
    diagnostics: list[Diagnostic] = field(default_factory=list)
    hashes: list[dict[str, object]] = field(default_factory=list)

    def to_json(self) -> dict[str, object]:
        return {
            "segment_sha256_count": self.count,
            "segment_sha256_mismatches": self.mismatches,
            "hashes": list(self.hashes),
            "diagnostics": [d.to_json() for d in self.diagnostics],
        }


def verify_segment_hashes(
    outputs: list[dict[str, object]],
) -> HashReport:
    """Recompute each output WAV hash and compare with the recorded value."""
    report = HashReport(count=0, mismatches=0)
    for output in sorted(outputs, key=lambda o: int(o["sequence"])):
        path = Path(str(output["output_path"]))
        recorded = output.get("sha256")
        if not path.exists():
            report.mismatches += 1
            report.diagnostics.append(
                Diagnostic(
                    Code.HASH_MISMATCH,
                    "output segment is missing on disk",
                    Locator(
                        sequence=int(output["sequence"]),
                        representation=str(output.get("representation", "")),
                        segment_ref=str(output.get("raw_ref", "")),
                    ),
                )
            )
            continue
        actual = _file_sha256(path)
        report.count += 1
        report.hashes.append(
            {
                "sequence": output["sequence"],
                "status": output.get("status"),
                "sha256": actual,
                "recorded_sha256": recorded,
            }
        )
        if actual != recorded:
            report.mismatches += 1
            report.diagnostics.append(
                Diagnostic(
                    Code.HASH_MISMATCH,
                    "recomputed hash differs from the recorded value",
                    Locator(
                        sequence=int(output["sequence"]),
                        representation=str(output.get("representation", "")),
                        segment_ref=str(output.get("raw_ref", "")),
                    ),
                    {"recorded": recorded, "actual": actual},
                )
            )
    return report


def timeline_preservation(
    plan: SegmentPlan, outputs: list[dict[str, object]], target_rate: int
) -> tuple[float, list[Diagnostic]]:
    """Confirm output durations cover planned durations (no silent compression).

    Returns the fraction of planned programme time covered by output samples
    plus diagnostics for any segment whose output is shorter than planned.
    """
    diagnostics: list[Diagnostic] = []
    by_sequence = {int(o["sequence"]): o for o in outputs}
    planned_total = 0
    covered = 0
    for segment in plan.segments:
        expected = int(round(rational(segment.duration) * target_rate))
        planned_total += expected
        output = by_sequence.get(segment.sequence)
        written = int(output["written_samples"]) if output else 0
        covered += min(written, expected)
        if written < expected:
            diagnostics.append(
                Diagnostic(
                    Code.TIMELINE_GAP,
                    "output is shorter than the planned segment duration",
                    Locator(
                        sequence=segment.sequence,
                        representation=segment.representation,
                        segment_ref=segment.raw_ref,
                        time_range=(
                            float(segment.start),
                            float(segment.end),
                        ),
                    ),
                    {"expected_samples": expected, "written_samples": written},
                )
            )
    coverage = covered / planned_total if planned_total else 1.0
    return coverage, diagnostics


__all__ = [
    "HashReport",
    "input_fingerprint",
    "roundtrip_consistency",
    "timeline_preservation",
    "verify_segment_hashes",
]
