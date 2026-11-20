"""HLS media-playlist parsing and round-trip rendering.

The shared plan dataclasses and reference normalization live in
:mod:`archive_media.manifest_core`; this module only knows the HLS tag
grammar.  Archive-specific tags (``#ARCHIVE-REP`` etc.) carry the
representation id, segment time base and explicit starts used by the
self-generated fixtures; unknown ``#EXT`` tags are ignored as the spec
requires.
"""

from __future__ import annotations

import posixpath
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .errors import Code, Diagnostic, Locator, ManifestSyntaxError
from .manifest_core import SegmentPlan, SegmentRef, _normalize_ref
from .timeline import rational, to_float_seconds

_HLS_PREFIX = "#EXT"
_ARCHIVE_REP = "#ARCHIVE-REP:"
_ARCHIVE_TIMEBASE = "#ARCHIVE-TIMEBASE:"
_ARCHIVE_START = "#ARCHIVE-START:"


def _locator(manifest: Path, rep: str, seq: int | None, ref: str = "") -> Locator:
    return Locator(
        manifest_path=str(manifest), representation=rep, sequence=seq, segment_ref=ref
    )


def parse_hls(text: str, manifest_path: Path, root: Path) -> SegmentPlan:
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    if not lines or lines[0] != "#EXTM3U":
        raise ManifestSyntaxError(
            Code.MANIFEST_SYNTAX, "HLS manifest must start with #EXTM3U",
            locator=_locator(manifest_path, "", None),
        )
    manifest_dir = manifest_path.parent
    rep = "default"
    time_base = Fraction(1, 90000)
    target_duration: Fraction | None = None
    media_sequence = 0
    pending_duration: Fraction | None = None
    pending_start: Fraction | None = None
    cursor = Fraction(0)
    seq = media_sequence - 1
    segments: list[SegmentRef] = []
    diagnostics: list[Diagnostic] = []
    seen_refs: dict[str, int] = {}

    for line in lines[1:]:
        if line.startswith(_ARCHIVE_REP):
            rep = line[len(_ARCHIVE_REP):].strip() or "default"
        elif line.startswith(_ARCHIVE_TIMEBASE):
            time_base = Fraction(1, int(line[len(_ARCHIVE_TIMEBASE):].strip()))
        elif line.startswith(_ARCHIVE_START):
            pending_start = rational(line[len(_ARCHIVE_START):].strip())
        elif line.startswith("#EXT-X-TARGETDURATION:"):
            target_duration = rational(line.split(":", 1)[1])
        elif line.startswith("#EXT-X-MEDIA-SEQUENCE:"):
            media_sequence = int(line.split(":", 1)[1])
            seq = media_sequence - 1
        elif line.startswith("#EXTINF:"):
            value = line.split(":", 1)[1].rstrip(",")
            pending_duration = rational(value.split(",", 1)[0])
        elif line.startswith(_HLS_PREFIX):
            continue
        else:
            seq += 1
            locator = _locator(manifest_path, rep, seq, line)
            resolved = _normalize_ref(line, manifest_dir, root, locator)
            synthesized = False
            if pending_duration is None:
                if target_duration is None:
                    raise ManifestSyntaxError(
                        Code.MANIFEST_SYNTAX,
                        "segment has no EXTINF and playlist has no target duration",
                        locator=locator,
                    )
                pending_duration = target_duration
                synthesized = True
            start = pending_start if pending_start is not None else cursor
            seg = SegmentRef(
                seq, line, resolved, rep, start, pending_duration, time_base,
                synthesized=synthesized,
            )
            if line in seen_refs:
                seg.duplicated = True
                seg.diagnostics.append(
                    Diagnostic(
                        Code.DUPLICATE_REFERENCE,
                        "segment URI is referenced more than once",
                        locator, {"first_sequence": seen_refs[line]},
                    )
                )
            else:
                seen_refs[line] = seq
            if synthesized:
                seg.diagnostics.append(
                    Diagnostic(
                        Code.MISSING_INDEX_ENTRY,
                        "segment lacks EXTINF; duration synthesized from target",
                        locator, {"duration_seconds": to_float_seconds(pending_duration)},
                    )
                )
            segments.append(seg)
            cursor = start + pending_duration
            pending_duration = None
            pending_start = None
    if not segments:
        raise ManifestSyntaxError(
            Code.MANIFEST_SYNTAX, "HLS manifest contains no segment references",
            locator=_locator(manifest_path, rep, None),
        )
    return SegmentPlan(
        "hls", manifest_path, root, sorted({s.representation for s in segments}),
        segments, diagnostics, text,
    )


def render_hls(plan: SegmentPlan) -> str:
    """Serialize a plan back to HLS text (round-trip counterpart of parse)."""
    lines = ["#EXTM3U", "#EXT-X-VERSION:3"]
    current_rep = ""
    for seg in plan.segments:
        if seg.representation != current_rep:
            lines.append(f"{_ARCHIVE_REP}{seg.representation}")
            current_rep = seg.representation
        lines.append(f"#EXTINF:{to_float_seconds(seg.duration):.6f},")
        lines.append(seg.raw_ref)
    return "\n".join(lines) + "\n"


__all__ = ["parse_hls", "render_hls"]
