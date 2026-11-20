"""Shared segment-plan model, reference normalization and format dispatch.

The plan is the single object handed to demux, timeline, index and verify, so
its fields are deliberately complete: identity (sequence / representation),
the exact rational timing, both the raw manifest reference and the resolved
on-disk path, and the diagnostics raised while planning.
"""

from __future__ import annotations

import posixpath
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .errors import Code, Diagnostic, Locator, ManifestSyntaxError
from .timeline import TimelinePlanner, rational, to_float_seconds


@dataclass
class SegmentRef:
    """One planned segment: identity, raw reference and exact timing."""

    sequence: int
    raw_ref: str
    resolved_path: Path
    representation: str
    start: Fraction
    duration: Fraction
    time_base: Fraction
    duplicated: bool = False
    synthesized: bool = False
    diagnostics: list[Diagnostic] = field(default_factory=list)

    @property
    def end(self) -> Fraction:
        return self.start + self.duration

    def to_json(self) -> dict[str, object]:
        return {
            "sequence": self.sequence,
            "raw_ref": self.raw_ref,
            "resolved_path": str(self.resolved_path),
            "representation": self.representation,
            "start_seconds": to_float_seconds(self.start),
            "duration_seconds": to_float_seconds(self.duration),
            "end_seconds": to_float_seconds(self.end),
            "time_base": str(self.time_base),
            "duplicated": self.duplicated,
            "synthesized": self.synthesized,
        }


@dataclass
class SegmentPlan:
    kind: str  # "hls" or "dash"
    manifest_path: Path
    root: Path
    representations: list[str]
    segments: list[SegmentRef]
    diagnostics: list[Diagnostic] = field(default_factory=list)
    source_text: str = ""
    # DASH-only: the original SegmentTemplate media pattern and start number,
    # retained so round-trip rendering reproduces the exact segment URIs.
    media_template: str = ""
    start_number: int | None = None

    def all_diagnostics(self) -> list[Diagnostic]:
        out = list(self.diagnostics)
        for seg in self.segments:
            out.extend(seg.diagnostics)
        return out

    def to_json(self) -> dict[str, object]:
        return {
            "kind": self.kind,
            "manifest_path": str(self.manifest_path),
            "root": str(self.root),
            "representations": list(self.representations),
            "segments": [s.to_json() for s in self.segments],
            "diagnostics": [d.to_json() for d in self.all_diagnostics()],
        }


def locator_for(
    manifest: Path, rep: str, seq: int | None, ref: str = ""
) -> Locator:
    return Locator(
        manifest_path=str(manifest), representation=rep, sequence=seq, segment_ref=ref
    )


def normalize_ref(raw: str, manifest_dir: Path, root: Path, locator: Locator) -> Path:
    """Resolve a manifest reference, refusing schemes and root escape.

    Normalization is purely lexical until the final ``resolve``; symlinks are
    not followed here, but the resolved path must still sit inside the archive
    root, which also defeats ``..`` traversal through symlinked directories.
    """
    if "://" in raw or raw.startswith(("file:", "//")):
        raise ManifestSyntaxError(
            Code.REFERENCE_ESCAPE, "network or scheme references are not allowed",
            locator=locator, detail={"ref": raw},
        )
    cleaned = posixpath.normpath(raw.strip())
    if cleaned == ".." or cleaned.startswith("../") or posixpath.isabs(cleaned):
        raise ManifestSyntaxError(
            Code.REFERENCE_ESCAPE, "reference escapes the archive root",
            locator=locator, detail={"ref": raw},
        )
    resolved = (manifest_dir / cleaned).resolve()
    if not resolved.is_relative_to(root.resolve()):
        raise ManifestSyntaxError(
            Code.REFERENCE_ESCAPE, "normalized reference escapes the archive root",
            locator=locator, detail={"ref": raw, "resolved": str(resolved)},
        )
    return resolved


# Underscore alias kept for the format-specific submodules.
_normalize_ref = normalize_ref


def validate_timeline(plan: SegmentPlan) -> list[Diagnostic]:
    """Lay every planned segment on one timeline and record discontinuities.

    The plan itself is never reordered or compressed here; the diagnostics
    describe reordering, timestamp-rounding collisions and non-contiguous
    joins so the runner and verifier can audit them.
    """
    diagnostics: list[Diagnostic] = []
    planners: dict[str, TimelinePlanner] = {}
    for segment in plan.segments:
        planner = planners.setdefault(
            segment.representation, TimelinePlanner(segment.time_base)
        )
        planner.add(
            segment.sequence,
            segment.start,
            segment.duration,
            representation=segment.representation,
            manifest_path=str(plan.manifest_path),
            segment_ref=segment.raw_ref,
        )
    for planner in planners.values():
        diagnostics.extend(planner.diagnostics)
    plan.diagnostics.extend(diagnostics)
    return diagnostics


def parse_manifest(path: str | Path, root: str | Path | None = None) -> SegmentPlan:
    """Dispatch HLS vs DASH by extension and parse the manifest at ``path``."""
    manifest_path = Path(path).resolve()
    root_path = Path(root).resolve() if root else manifest_path.parent
    text = manifest_path.read_text(encoding="utf-8")
    suffix = manifest_path.suffix.lower()
    if suffix in {".m3u8", ".m3u"}:
        from .manifest_hls import parse_hls

        plan = parse_hls(text, manifest_path, root_path)
    elif suffix == ".mpd":
        from .manifest_dash import parse_dash

        plan = parse_dash(text, manifest_path, root_path)
    else:
        raise ManifestSyntaxError(
            Code.MANIFEST_UNSUPPORTED,
            f"unsupported manifest extension: {manifest_path.name}",
            locator=locator_for(manifest_path, "", None),
        )
    validate_timeline(plan)
    return plan


__all__ = [
    "SegmentPlan",
    "SegmentRef",
    "locator_for",
    "normalize_ref",
    "parse_manifest",
    "validate_timeline",
]
