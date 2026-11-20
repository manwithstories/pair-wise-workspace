"""Video colour-metadata validation.

Audio processing must never silently rewrite the video timeline, and colour
metadata must never be *converted*: this module only verifies what the edge
encoders declared, keeps the original values verbatim, and records missing or
conflicting declarations in the shared error model.

The four keys mirror FFmpeg's ``AVFrame`` colour fields plus range, so the
report can be matched against a real ffprobe dump without translation.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from . import packet
from .errors import Code, Diagnostic

VIDEO_COLOUR_KEYS: tuple[str, ...] = (
    "color_space",
    "color_primaries",
    "color_transfer",
    "color_range",
)

# Values that mean "unspecified" in FFmpeg and should count as absent.
_UNSPECIFIED = {"", "unknown", "unspecified", "none", "0", "2"}


@dataclass
class VideoMetadataReport:
    """Per-run colour-metadata verdict, preserved as declared."""

    records: int = 0
    missing_fields: list[str] = field(default_factory=list)
    conflicts: list[dict[str, Any]] = field(default_factory=list)
    preserved: dict[str, Any] = field(default_factory=dict)
    time_ranges: list[tuple[int, int]] = field(default_factory=list)

    @property
    def missing(self) -> int:
        return len(self.missing_fields)

    @property
    def conflict_count(self) -> int:
        return len(self.conflicts)

    @property
    def complete(self) -> bool:
        return not self.missing_fields and not self.conflicts

    def to_json(self) -> dict[str, Any]:
        return {
            "records": self.records,
            "missing": self.missing,
            "missing_fields": list(self.missing_fields),
            "conflicts": self.conflict_count,
            "conflict_details": list(self.conflicts),
            "preserved": dict(self.preserved),
            "complete": self.complete,
        }


def _is_declared(value: Any) -> bool:
    if value is None:
        return False
    if isinstance(value, str) and value.strip().lower() in _UNSPECIFIED:
        return False
    return True


def inspect_video_metadata(
    videos: Iterable[packet.VideoRecord], *, where: str = ""
) -> tuple[VideoMetadataReport, list[Diagnostic]]:
    """Check colour metadata across all video records of one capture.

    Values are never rewritten; the first declared value per key is kept as
    ``preserved`` and every record that disagrees is listed as a conflict.
    """
    report = VideoMetadataReport()
    diags: list[Diagnostic] = []
    declared: dict[str, tuple[Any, int]] = {}
    missing: set[str] = set()
    for video in videos:
        report.records += 1
        report.time_ranges.append(
            (video.pts_samples, video.pts_samples + video.duration_samples)
        )
        for key in VIDEO_COLOUR_KEYS:
            value = video.metadata.get(key)
            if not _is_declared(value):
                missing.add(key)
                continue
            if key not in declared:
                declared[key] = (value, video.index)
            elif declared[key][0] != value:
                report.conflicts.append(
                    {
                        "key": key,
                        "first": declared[key][0],
                        "first_record": declared[key][1],
                        "value": value,
                        "record": video.index,
                    }
                )
    for key in VIDEO_COLOUR_KEYS:
        if key in missing and key not in declared:
            report.missing_fields.append(key)
    report.preserved = {key: value for key, (value, _) in declared.items()}
    if not report.records:
        diags.append(
            Diagnostic(
                Code.METADATA_MISSING,
                "capture has no video records to validate",
                where,
            )
        )
    else:
        for key in sorted(missing):
            diags.append(
                Diagnostic(
                    Code.METADATA_MISSING,
                    f"colour metadata '{key}' missing on some video records",
                    where,
                    {"key": key},
                )
            )
        for conflict in report.conflicts:
            diags.append(
                Diagnostic(
                    Code.METADATA_CONFLICT,
                    f"colour metadata '{conflict['key']}' conflicts",
                    where,
                    conflict,
                )
            )
    return report, diags


def timeline_shift_diagnostics(
    report: VideoMetadataReport,
    *,
    source_records: Iterable[packet.VideoRecord],
    output_records: Iterable[dict[str, Any]],
    clock_rate: int,
) -> list[Diagnostic]:
    """Verify the video timeline was copied through unchanged.

    Audio processing may insert silence or resample, but it must never move a
    video frame.  The manifest carries the video records verbatim, so the only
    truthful check is: does every ``(pts_samples, duration_samples)`` on the
    output side still match the source?  Any drift is reported as
    ``ERR_VIDEO_TIME_SHIFT`` with the exact sample offset.
    """
    source = [
        (int(v.pts_samples), int(v.duration_samples)) for v in source_records
    ]
    output = [
        (int(r.get("pts_samples", 0)), int(r.get("duration_samples", 0)))
        for r in output_records
    ]
    diags: list[Diagnostic] = []
    if len(source) != len(output):
        diags.append(
            Diagnostic(
                Code.VIDEO_TIME_SHIFT,
                "video record count changed during processing",
                "",
                {"source_records": len(source), "output_records": len(output)},
            )
        )
        return diags
    for index, (before, after) in enumerate(zip(source, output)):
        if before != after:
            diags.append(
                Diagnostic(
                    Code.VIDEO_TIME_SHIFT,
                    "video timing changed during audio processing",
                    "",
                    {
                        "index": index,
                        "source_pts_samples": before[0],
                        "output_pts_samples": after[0],
                        "source_duration": before[1],
                        "output_duration": after[1],
                        "clock_rate": clock_rate,
                    },
                )
            )
    return diags


def video_output_records(
    videos: Iterable[packet.VideoRecord],
) -> list[dict[str, Any]]:
    """Serialise video records unchanged for the manifest."""
    return [video.to_json() for video in videos]


__all__ = [
    "VIDEO_COLOUR_KEYS",
    "VideoMetadataReport",
    "inspect_video_metadata",
    "timeline_shift_diagnostics",
    "video_output_records",
]
