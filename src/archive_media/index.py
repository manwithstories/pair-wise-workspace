"""Keyframe / decodable-point index with neighbour-scan recovery.

Audio codecs such as AAC make every frame independently decodable, so a
"keyframe" is any frame at which decoding can (re)start.  When an imported
segment carries no explicit index entries, :func:`scan_decodable_points`
walks the demuxed frames and promotes frame boundaries whose PTS is a valid
decode start; the scan is local (a bounded neighbour window) rather than a
full re-decode, keeping cost linear.

The index is a deterministic JSON document: entries are sorted by time and
duplicate points are collapsed, so re-running an unchanged input produces a
byte-identical file.  Verification reports a hit rate against a millisecond
tolerance, and every miss is a located
:data:`~archive_media.errors.Code.INDEX_MISS` diagnostic rather than a silent
drop.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .errors import Code, Diagnostic, Locator
from .timeline import MILLISECOND, rational, to_float_seconds

INDEX_NAME = "keyframe_index.json"
NEIGHBOUR_WINDOW = 2
INDEX_VERSION = 1


@dataclass(frozen=True)
class IndexEntry:
    sequence: int
    pts_seconds: Fraction
    offset_samples: int
    keyframe: bool

    def to_json(self) -> dict[str, object]:
        return {
            "sequence": self.sequence,
            "pts_seconds": to_float_seconds(self.pts_seconds),
            "offset_samples": self.offset_samples,
            "keyframe": self.keyframe,
        }


@dataclass
class KeyframeIndex:
    representation: str
    entries: list[IndexEntry] = field(default_factory=list)

    def sorted_points(self) -> list[Fraction]:
        return sorted({e.pts_seconds for e in self.entries})

    def to_json(self) -> dict[str, object]:
        ordered = sorted(self.entries, key=lambda e: (e.pts_seconds, e.sequence))
        return {
            "version": INDEX_VERSION,
            "representation": self.representation,
            "entry_count": len(ordered),
            "entries": [e.to_json() for e in ordered],
        }

    def write(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(path.name + ".part")
        tmp.write_text(
            json.dumps(self.to_json(), indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        tmp.replace(path)


def load_index(path: Path) -> KeyframeIndex:
    data = json.loads(path.read_text(encoding="utf-8"))
    if data.get("version") != INDEX_VERSION:
        raise ValueError(f"{path} is not a version-{INDEX_VERSION} keyframe index")
    entries = [
        IndexEntry(
            sequence=int(item["sequence"]),
            pts_seconds=rational(item["pts_seconds"]),
            offset_samples=int(item["offset_samples"]),
            keyframe=bool(item["keyframe"]),
        )
        for item in data["entries"]
    ]
    return KeyframeIndex(str(data["representation"]), entries)


def scan_decodable_points(
    sequence: int,
    frame_pts: list[Fraction],
    frame_sample_offsets: list[int],
    keyframe_flags: list[bool],
    *,
    explicit_index: list[Fraction] | None = None,
) -> tuple[list[IndexEntry], list[Diagnostic]]:
    """Build index entries for one segment, scanning neighbours when incomplete.

    ``explicit_index`` is the set of PTS values the source claimed; if it is
    missing or empty, every frame boundary is a candidate decodable point and
    the nearest neighbours of each claimed gap are promoted.  Diagnostics
    record which entries had to be synthesized so coverage stays auditable.
    """
    diagnostics: list[Diagnostic] = []
    locator = Locator(sequence=sequence)
    explicit = {rational(p) for p in (explicit_index or [])}
    entries: list[IndexEntry] = []
    seen: set[tuple[int, Fraction]] = set()
    if not explicit:
        diagnostics.append(
            Diagnostic(
                Code.MISSING_INDEX_ENTRY,
                "no explicit keyframe index; neighbour scan promoted frame starts",
                locator,
                {"window": NEIGHBOUR_WINDOW, "frames": len(frame_pts)},
            )
        )
    for i, (pts, offset) in enumerate(zip(frame_pts, frame_sample_offsets)):
        pts = rational(pts)
        claimed = pts in explicit
        # Promote a frame if it is claimed, or is within the neighbour window
        # of a claimed point, or no explicit index exists at all.
        promote = claimed or not explicit
        if not promote and explicit:
            for j in range(max(0, i - NEIGHBOUR_WINDOW),
                           min(len(frame_pts), i + NEIGHBOUR_WINDOW + 1)):
                if frame_pts[j] in explicit:
                    promote = True
                    break
        is_key = bool(keyframe_flags[i]) if i < len(keyframe_flags) else False
        if promote and (sequence, pts) not in seen:
            seen.add((sequence, pts))
            entries.append(IndexEntry(sequence, pts, offset, is_key or not explicit))
    return entries, diagnostics


def verify_hit_rate(
    index: KeyframeIndex,
    expected_points: list[Fraction],
    tolerance_ms: float,
) -> tuple[float, list[Diagnostic]]:
    """Fraction of expected decodable points matched within the tolerance."""
    points = index.sorted_points()
    if not expected_points:
        return 1.0, []
    tolerance = rational(tolerance_ms) * MILLISECOND
    diagnostics: list[Diagnostic] = []
    hits = 0
    for expected in expected_points:
        expected = rational(expected)
        if points and min(abs(expected - p) for p in points) <= tolerance:
            hits += 1
        else:
            diagnostics.append(
                Diagnostic(
                    Code.INDEX_MISS,
                    "no keyframe entry within the time tolerance",
                    Locator(
                        representation=index.representation,
                        time_range=(to_float_seconds(expected),
                                    to_float_seconds(expected)),
                    ),
                    {"tolerance_ms": tolerance_ms,
                     "expected_seconds": to_float_seconds(expected)},
                )
            )
    return hits / len(expected_points), diagnostics


def build_index_from_results(
    decode_results: list[object],
) -> tuple[KeyframeIndex, list[Diagnostic]]:
    """Turn :class:`~archive_media.demux.SegmentDecodeResult` objects into an index."""
    all_entries: list[IndexEntry] = []
    diagnostics: list[Diagnostic] = []
    representation = "default"
    for result in decode_results:
        representation = result.segment.representation
        pts: list[Fraction] = []
        offsets: list[int] = []
        flags: list[bool] = []
        offset = 0
        for chunk in result.chunks:
            if chunk.pts_seconds is not None:
                pts.append(rational(chunk.pts_seconds))
                offsets.append(offset)
                flags.append(chunk.keyframe)
            offset += int(chunk.samples.shape[0])
        entries, diags = scan_decodable_points(
            result.segment.sequence, pts, offsets, flags,
            explicit_index=result.keyframe_pts or None,
        )
        all_entries.extend(entries)
        diagnostics.extend(diags)
    return KeyframeIndex(representation, all_entries), diagnostics


__all__ = [
    "INDEX_NAME",
    "IndexEntry",
    "KeyframeIndex",
    "build_index_from_results",
    "load_index",
    "scan_decodable_points",
    "verify_hit_rate",
]
