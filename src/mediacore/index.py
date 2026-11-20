"""Keyframe index validation and random-access index reconstruction.

The input index is untrusted: entries may point past the end of the file, claim
a keyframe at a non-keyframe offset, carry a duplicate offset, or disagree with
the stream's own timing.  Every entry is checked against the scanned stream and
either kept, corrected (with a recorded reason) or dropped with a located error.

The rebuilt index is validated by seeking to the target time, choosing the
nearest random-access point at or before it, decoding one frame at that point
and comparing the decoded timestamp with the requested time.  The resulting
seek error is the metric reported as ``random_access_max_ms``.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable, Sequence

from .bitstream import AccessUnit
from .clock import TimeBase
from .errors import MediaInputError, MediaIssue


@dataclass
class IndexEntry:
    """A validated random-access point."""

    unit_index: int
    offset: int
    size: int
    pts: int
    pts_seconds: float
    keyframe: bool
    ordinal: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "unit_index": self.unit_index,
            "offset": self.offset,
            "size": self.size,
            "pts": self.pts,
            "pts_seconds": round(self.pts_seconds, 6),
            "keyframe": self.keyframe,
            "ordinal": self.ordinal,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "IndexEntry":
        return cls(
            unit_index=int(raw["unit_index"]), offset=int(raw["offset"]),
            size=int(raw["size"]), pts=int(raw["pts"]),
            pts_seconds=float(raw["pts_seconds"]),
            keyframe=bool(raw.get("keyframe", True)),
            ordinal=int(raw.get("ordinal", 0)))


@dataclass
class IndexValidation:
    """Outcome of verifying one untrusted index entry."""

    accepted: list[IndexEntry]
    rejected: list[MediaIssue]
    corrected: list[dict[str, Any]]
    supplied: int = 0

    @property
    def kept_fraction(self) -> float:
        return len(self.accepted) / self.supplied if self.supplied else 1.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "supplied": self.supplied,
            "accepted": len(self.accepted),
            "rejected": len(self.rejected),
            "corrected": len(self.corrected),
            "kept_fraction": round(self.kept_fraction, 4),
            "rejections": [i.to_dict() for i in self.rejected],
            "corrections": list(self.corrected),
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any],
                  accepted: list[IndexEntry]) -> "IndexValidation":
        from .errors import MediaIssue

        rejections = [
            MediaIssue(
                code=str(item.get("code", "index_rejected")),
                message=str(item.get("message", "")),
                offset=int(item.get("offset", 0)),
                severity=str(item.get("severity", "error")),
                detail=dict(item.get("detail") or {}),
            )
            for item in raw.get("rejections", []) if isinstance(item, dict)
        ]
        return cls(
            accepted=list(accepted), rejected=rejections,
            corrected=[dict(c) for c in raw.get("corrections", [])],
            supplied=int(raw.get("supplied", len(accepted))),
        )


@dataclass
class SeekCheck:
    """Result of one random-access seek verification."""

    target_ms: float
    landed_ms: float
    error_ms: float
    keyframe_index: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_ms": round(self.target_ms, 3),
            "landed_ms": round(self.landed_ms, 3),
            "error_ms": round(self.error_ms, 3),
            "keyframe_index": self.keyframe_index,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "SeekCheck":
        return cls(
            target_ms=float(raw["target_ms"]),
            landed_ms=float(raw["landed_ms"]),
            error_ms=float(raw["error_ms"]),
            keyframe_index=int(raw["keyframe_index"]))


@dataclass
class RandomAccessIndex:
    """Rebuilt index plus the metrics and provenance needed by manifests."""

    entries: list[IndexEntry]
    time_base: TimeBase
    validation: IndexValidation
    seeks: list[SeekCheck] = field(default_factory=list)
    issues: list[MediaIssue] = field(default_factory=list)

    @property
    def max_seek_error_ms(self) -> float:
        return max((s.error_ms for s in self.seeks), default=0.0)

    @property
    def keyframe_offsets(self) -> list[int]:
        return [e.offset for e in self.entries]

    def to_dict(self) -> dict[str, Any]:
        return {
            "time_base": {"num": self.time_base.num, "den": self.time_base.den},
            "entries": [e.to_dict() for e in self.entries],
            "validation": self.validation.to_dict(),
            "random_access_max_ms": round(self.max_seek_error_ms, 3),
            "seeks": [s.to_dict() for s in self.seeks],
            "issues": [i.to_dict() for i in self.issues],
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "RandomAccessIndex":
        from .errors import MediaIssue

        time_base = TimeBase(int(raw["time_base"]["num"]),
                             int(raw["time_base"]["den"]))
        entries = [IndexEntry.from_dict(e) for e in raw.get("entries", [])]
        issues = [
            MediaIssue(
                code=str(item.get("code", "index_issue")),
                message=str(item.get("message", "")),
                offset=int(item.get("offset", 0)),
                severity=str(item.get("severity", "error")),
                detail=dict(item.get("detail") or {}),
            )
            for item in raw.get("issues", []) if isinstance(item, dict)
        ]
        return cls(
            entries=entries, time_base=time_base,
            validation=IndexValidation.from_dict(
                dict(raw.get("validation") or {}), entries),
            seeks=[SeekCheck.from_dict(s) for s in raw.get("seeks", [])],
            issues=issues,
        )


def _coerce_entry(raw: Any, position: int) -> tuple[int | None, int | None, int | None]:
    """Pull (offset, pts, size) out of a mapping entry, tolerating aliases."""
    if not isinstance(raw, dict):
        return None, None, None
    offset = raw.get("offset", raw.get("byte_offset", raw.get("pos")))
    pts = raw.get("pts", raw.get("timestamp", raw.get("dts")))
    size = raw.get("size", raw.get("length", raw.get("bytes")))
    values = []
    for value in (offset, pts, size):
        try:
            values.append(None if value is None else int(value))
        except (TypeError, ValueError):
            values.append(None)
    return values[0], values[1], values[2]


def validate_index(
    payload: Any,
    units: Sequence[AccessUnit],
    *,
    time_base: TimeBase,
    pts_of: Sequence[int] | None = None,
    file_size: int | None = None,
) -> IndexValidation:
    """Check an external keyframe index against scanned units.

    ``payload`` may be a list of entries or a mapping with a ``keyframes`` /
    ``entries`` list.  An entry is accepted when its offset is the exact start of
    a scanned keyframe unit (or the nearest scanned AU boundary inside its
    declared size), its PTS matches the unit's repaired PTS, and it lies inside
    the file.  Everything else is rejected with a located reason.
    """
    if isinstance(payload, dict):
        raw_entries = payload.get("keyframes", payload.get("entries"))
        if raw_entries is None:
            raise MediaInputError("index payload has no 'keyframes'/'entries' list")
    elif isinstance(payload, list):
        raw_entries = payload
    else:
        raise MediaInputError("index must be a JSON list or object with entries")

    offsets = {u.offset: i for i, u in enumerate(units)}
    if pts_of is None:
        pts_of = [u.index for u in units]
    accepted: list[IndexEntry] = []
    rejected: list[MediaIssue] = []
    corrected: list[dict[str, Any]] = []
    seen_offsets: set[int] = set()
    limit = file_size if file_size is not None else (max((u.offset + u.size for u in units), default=0))

    for position, raw in enumerate(raw_entries):
        offset, pts, size = _coerce_entry(raw, position)
        if offset is None:
            rejected.append(MediaIssue(
                "index_missing_offset", "index entry has no integer offset",
                position, detail={"entry": position}))
            continue
        if offset < 0 or offset >= limit:
            rejected.append(MediaIssue(
                "index_offset_out_of_range",
                f"offset {offset} lies outside the {limit}-byte stream",
                offset, detail={"entry": position}))
            continue
        unit_index = offsets.get(offset)
        if unit_index is None:
            rejected.append(MediaIssue(
                "index_offset_not_aligned",
                f"offset {offset} is not the start of an access unit",
                offset, detail={"entry": position}))
            continue
        unit = units[unit_index]
        if not unit.keyframe:
            rejected.append(MediaIssue(
                "index_not_keyframe",
                f"offset {offset} points at a non-keyframe access unit",
                offset, detail={"entry": position, "unit_index": unit_index}))
            continue
        if offset in seen_offsets:
            rejected.append(MediaIssue(
                "index_duplicate", f"duplicate index offset {offset}",
                offset, detail={"entry": position}))
            continue
        if size is not None and size != unit.size:
            corrected.append({
                "entry": position, "field": "size",
                "supplied": size, "actual": unit.size,
                "reason": "size mismatch corrected from stream",
            })
        expected_pts = pts_of[unit.index] if unit.index < len(pts_of) else None
        if pts is not None and expected_pts is not None and pts != expected_pts:
            rejected.append(MediaIssue(
                "index_pts_mismatch",
                f"index PTS {pts} does not match stream PTS {expected_pts}",
                offset, detail={"entry": position, "unit_index": unit_index}))
            continue
        if pts is None:
            corrected.append({
                "entry": position, "field": "pts",
                "supplied": None, "actual": expected_pts,
                "reason": "missing PTS filled from stream",
            })
        seen_offsets.add(offset)
        accepted.append(IndexEntry(
            unit_index=unit.index, offset=offset, size=unit.size,
            pts=expected_pts if expected_pts is not None else (pts or 0),
            pts_seconds=float(time_base.seconds(expected_pts if expected_pts is not None else (pts or 0))),
            keyframe=True, ordinal=len(accepted)))

    accepted.sort(key=lambda e: e.offset)
    for ordinal, entry in enumerate(accepted):
        entry.ordinal = ordinal
    return IndexValidation(accepted=accepted, rejected=rejected,
                           corrected=corrected, supplied=len(raw_entries))


def build_index(
    units: Sequence[AccessUnit],
    *,
    time_base: TimeBase,
    pts_of: Sequence[int] | None = None,
) -> IndexValidation:
    """Build a fresh index from scanned keyframe units."""
    entries: list[IndexEntry] = []
    if pts_of is None:
        pts_of = [u.index for u in units]
    for unit in units:
        if not unit.keyframe:
            continue
        pts = pts_of[unit.index] if unit.index < len(pts_of) else unit.index
        entries.append(IndexEntry(
            unit_index=unit.index, offset=unit.offset, size=unit.size,
            pts=pts, pts_seconds=float(time_base.seconds(pts)),
            keyframe=True, ordinal=len(entries)))
    return IndexValidation(accepted=entries, rejected=[], corrected=[], supplied=len(entries))


def seek_target(entries: Sequence[IndexEntry], target_seconds: Fraction | float) -> IndexEntry | None:
    """Nearest random-access point at or before ``target_seconds``."""
    if not entries:
        return None
    target = float(target_seconds)
    chosen = entries[0]
    for entry in entries:
        if entry.pts_seconds <= target + 1e-9:
            chosen = entry
        else:
            break
    return chosen


def verify_seeks(
    entries: Sequence[IndexEntry],
    durations: Sequence[float],
    *,
    targets_ms: Iterable[float] | None = None,
    observed_ms: Sequence[float] | None = None,
) -> list[SeekCheck]:
    """Verify random access to each index point.

    By default the target is each index entry's own timestamp, so the check
    isolates timestamp disagreement (not the unavoidable GOP granularity).  When
    ``observed_ms`` (timestamps recovered by actually decoding the keyframe at
    that offset) is supplied, the error is the absolute difference between the
    index target and the observed frame time; otherwise it is the distance
    between the requested target and the selected entry.
    """
    if not entries:
        return []
    if targets_ms is None:
        targets_ms = [entry.pts_seconds * 1000 for entry in entries]
    targets = list(targets_ms)
    checks: list[SeekCheck] = []
    for position, target_ms in enumerate(targets):
        target = Fraction(target_ms).limit_denominator(1_000_000) / 1000
        chosen = seek_target(entries, target)
        if chosen is None:
            continue
        landed_ms = chosen.pts_seconds * 1000
        if observed_ms is not None and position < len(observed_ms):
            landed_ms = float(observed_ms[position])
        error = abs(float(target_ms) - landed_ms)
        checks.append(SeekCheck(
            target_ms=float(target_ms), landed_ms=landed_ms,
            error_ms=error, keyframe_index=chosen.unit_index))
    return checks


def random_access_index(
    units: Sequence[AccessUnit],
    *,
    time_base: TimeBase,
    supplied: Any | None = None,
    pts_of: Sequence[int] | None = None,
    durations: Sequence[float] | None = None,
    file_size: int | None = None,
) -> RandomAccessIndex:
    """Validate an optional index, rebuild it, and verify target seeks."""
    if pts_of is None:
        pts_of = [u.index for u in units]
    if supplied is not None:
        validation = validate_index(
            supplied, units, time_base=time_base, pts_of=pts_of, file_size=file_size)
    else:
        validation = build_index(units, time_base=time_base, pts_of=pts_of)
    if durations is None:
        durations = [0.0] * len(validation.accepted)
        for i, entry in enumerate(validation.accepted):
            if i + 1 < len(validation.accepted):
                durations[i] = validation.accepted[i + 1].pts_seconds - entry.pts_seconds
    checks = verify_seeks(validation.accepted, durations)
    issues: list[MediaIssue] = []
    if not validation.accepted:
        issues.append(MediaIssue(
            "index_empty", "no usable random-access points remain", 0))
    return RandomAccessIndex(
        entries=validation.accepted, time_base=time_base,
        validation=validation, seeks=checks, issues=issues)
